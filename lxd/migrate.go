// Package migration provides the primitives for migration in LXD.
//
// See https://github.com/lxc/lxd/blob/master/specs/migration.md for a complete
// description.

package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"gopkg.in/lxc/go-lxc.v2"

	"github.com/lxc/lxd"
	"github.com/lxc/lxd/shared"
)

type migrationFields struct {
	live bool

	controlSecret string
	controlConn   *websocket.Conn

	criuSecret string
	criuConn   *websocket.Conn

	fsSecret string
	fsConn   *websocket.Conn

	container container
}

func (c *migrationFields) send(m proto.Message) error {
	w, err := c.controlConn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return err
	}
	defer w.Close()

	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}

	return shared.WriteAll(w, data)
}

func findCriu(host string) error {
	_, err := exec.LookPath("criu")
	if err != nil {
		return fmt.Errorf("CRIU is required for live migration but its binary couldn't be found on the %s server. Is it installed in LXD's path?", host)
	}

	return nil
}

func (c *migrationFields) recv(m proto.Message) error {
	mt, r, err := c.controlConn.NextReader()
	if err != nil {
		return err
	}

	if mt != websocket.BinaryMessage {
		return fmt.Errorf("Only binary messages allowed")
	}

	buf, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	return proto.Unmarshal(buf, m)
}

func (c *migrationFields) disconnect() {
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	if c.controlConn != nil {
		c.controlConn.WriteMessage(websocket.CloseMessage, closeMsg)
	}

	if c.fsConn != nil {
		c.fsConn.WriteMessage(websocket.CloseMessage, closeMsg)
	}

	if c.criuConn != nil {
		c.criuConn.WriteMessage(websocket.CloseMessage, closeMsg)
	}
}

func (c *migrationFields) sendControl(err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}

	msg := MigrationControl{
		Success: proto.Bool(err == nil),
		Message: proto.String(message),
	}
	c.send(&msg)

	if err != nil {
		c.disconnect()
	}
}

func (c *migrationFields) controlChannel() <-chan MigrationControl {
	ch := make(chan MigrationControl)
	go func() {
		msg := MigrationControl{}
		err := c.recv(&msg)
		if err != nil {
			shared.Debugf("Got error reading migration control socket %s", err)
			close(ch)
			return
		}
		ch <- msg
	}()

	return ch
}

func CollectCRIULogFile(c container, imagesDir string, function string, method string) error {
	t := time.Now().Format(time.RFC3339)
	newPath := shared.LogPath(c.Name(), fmt.Sprintf("%s_%s_%s.log", function, method, t))
	return shared.FileCopy(filepath.Join(imagesDir, fmt.Sprintf("%s.log", method)), newPath)
}

func GetCRIULogErrors(imagesDir string, method string) string {
	f, err := os.Open(path.Join(imagesDir, fmt.Sprintf("%s.log", method)))
	if err != nil {
		return fmt.Sprintf("Problem accessing CRIU log: %s", err)
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	ret := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Error") {
			ret = append(ret, scanner.Text())
		}
	}

	return strings.Join(ret, "\n")
}

type migrationSourceWs struct {
	migrationFields

	allConnected chan bool
}

func NewMigrationSource(c container) (*migrationSourceWs, error) {
	ret := migrationSourceWs{migrationFields{container: c}, make(chan bool, 1)}

	var err error
	ret.controlSecret, err = shared.RandomCryptoString()
	if err != nil {
		return nil, err
	}

	ret.fsSecret, err = shared.RandomCryptoString()
	if err != nil {
		return nil, err
	}

	if c.IsRunning() {
		if err := findCriu("source"); err != nil {
			return nil, err
		}

		ret.live = true
		ret.criuSecret, err = shared.RandomCryptoString()
		if err != nil {
			return nil, err
		}
	} else {
		c.StorageStart()
	}

	return &ret, nil
}

func (s *migrationSourceWs) Metadata() interface{} {
	secrets := shared.Jmap{
		"control": s.controlSecret,
		"fs":      s.fsSecret,
	}

	if s.criuSecret != "" {
		secrets["criu"] = s.criuSecret
	}

	return secrets
}

func (s *migrationSourceWs) Connect(op *operation, r *http.Request, w http.ResponseWriter) error {
	secret := r.FormValue("secret")
	if secret == "" {
		return fmt.Errorf("missing secret")
	}

	var conn **websocket.Conn

	switch secret {
	case s.controlSecret:
		conn = &s.controlConn
	case s.criuSecret:
		conn = &s.criuConn
	case s.fsSecret:
		conn = &s.fsConn
	default:
		/* If we didn't find the right secret, the user provided a bad one,
		 * which 403, not 404, since this operation actually exists */
		return os.ErrPermission
	}

	c, err := shared.WebsocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}

	*conn = c

	if s.controlConn != nil && (!s.live || s.criuConn != nil) && s.fsConn != nil {
		s.allConnected <- true
	}

	return nil
}

func (s *migrationSourceWs) Do(op *operation) error {
	<-s.allConnected

	criuType := CRIUType_CRIU_RSYNC.Enum()
	if !s.live {
		criuType = nil
		defer s.container.StorageStop()
	}

	idmaps := make([]*IDMapType, 0)

	idmapset := s.container.IdmapSet()
	if idmapset != nil {
		for _, ctnIdmap := range idmapset.Idmap {
			idmap := IDMapType{
				Isuid:    proto.Bool(ctnIdmap.Isuid),
				Isgid:    proto.Bool(ctnIdmap.Isgid),
				Hostid:   proto.Int(ctnIdmap.Hostid),
				Nsid:     proto.Int(ctnIdmap.Nsid),
				Maprange: proto.Int(ctnIdmap.Maprange),
			}

			idmaps = append(idmaps, &idmap)
		}
	}

	header := MigrationHeader{
		Fs:    MigrationFSType_RSYNC.Enum(),
		Criu:  criuType,
		Idmap: idmaps,
	}

	if err := s.send(&header); err != nil {
		s.sendControl(err)
		return err
	}

	if err := s.recv(&header); err != nil {
		s.sendControl(err)
		return err
	}

	if *header.Fs != MigrationFSType_RSYNC {
		err := fmt.Errorf("Formats other than rsync not understood")
		s.sendControl(err)
		return err
	}

	if s.live {
		if header.Criu == nil {
			err := fmt.Errorf("Got no CRIU socket type for live migration")
			s.sendControl(err)
			return err
		} else if *header.Criu != CRIUType_CRIU_RSYNC {
			err := fmt.Errorf("Formats other than criu rsync not understood")
			s.sendControl(err)
			return err
		}

		checkpointDir, err := ioutil.TempDir("", "lxd_migration_")
		if err != nil {
			s.sendControl(err)
			return err
		}
		defer os.RemoveAll(checkpointDir)

		opts := lxc.CheckpointOptions{Stop: true, Directory: checkpointDir, Verbose: true}
		err = s.container.Checkpoint(opts)

		if err2 := CollectCRIULogFile(s.container, checkpointDir, "migration", "dump"); err2 != nil {
			shared.Debugf("Error collecting checkpoint log file %s", err)
		}

		if err != nil {
			log := GetCRIULogErrors(checkpointDir, "dump")

			err = fmt.Errorf("checkpoint failed:\n%s", log)
			s.sendControl(err)
			return err
		}

		/*
		 * We do the serially right now, but there's really no reason for us
		 * to; since we have separate websockets, we can do it in parallel if
		 * we wanted to. However, assuming we're network bound, there's really
		 * no reason to do these in parallel. In the future when we're using
		 * p.haul's protocol, it will make sense to do these in parallel.
		 */
		if err := RsyncSend(shared.AddSlash(checkpointDir), s.criuConn); err != nil {
			s.sendControl(err)
			return err
		}
	}

	fsDir := s.container.RootfsPath()
	if err := RsyncSend(shared.AddSlash(fsDir), s.fsConn); err != nil {
		s.sendControl(err)
		return err
	}

	msg := MigrationControl{}
	if err := s.recv(&msg); err != nil {
		s.disconnect()
		return err
	}

	// TODO: should we add some config here about automatically restarting
	// the container migrate failure? What about the failures above?
	if !*msg.Success {
		return fmt.Errorf(*msg.Message)
	}

	return nil
}

type migrationSink struct {
	migrationFields

	url    string
	dialer websocket.Dialer
}

type MigrationSinkArgs struct {
	Url       string
	Dialer    websocket.Dialer
	Container container
	Secrets   map[string]string
}

func NewMigrationSink(args *MigrationSinkArgs) (func() error, error) {
	sink := migrationSink{
		migrationFields{container: args.Container},
		args.Url,
		args.Dialer,
	}

	var ok bool
	sink.controlSecret, ok = args.Secrets["control"]
	if !ok {
		return nil, fmt.Errorf("Missing control secret")
	}

	sink.fsSecret, ok = args.Secrets["fs"]
	if !ok {
		return nil, fmt.Errorf("Missing fs secret")
	}

	sink.criuSecret, ok = args.Secrets["criu"]
	sink.live = ok

	if err := findCriu("destination"); sink.live && err != nil {
		return nil, err
	}

	return sink.do, nil
}

func (c *migrationSink) connectWithSecret(secret string) (*websocket.Conn, error) {
	query := url.Values{"secret": []string{secret}}

	// TODO: we shouldn't assume this is a HTTP URL
	url := c.url + "?" + query.Encode()

	return lxd.WebsocketDial(c.dialer, url)
}

func (c *migrationSink) do() error {
	var err error
	c.controlConn, err = c.connectWithSecret(c.controlSecret)
	if err != nil {
		return err
	}
	defer c.disconnect()

	c.fsConn, err = c.connectWithSecret(c.fsSecret)
	if err != nil {
		c.sendControl(err)
		return err
	}

	if c.live {
		c.criuConn, err = c.connectWithSecret(c.criuSecret)
		if err != nil {
			c.sendControl(err)
			return err
		}
	}

	// For now, we just ignore whatever the server sends us. We only
	// support RSYNC, so that's what we respond with.
	header := MigrationHeader{}
	if err := c.recv(&header); err != nil {
		c.sendControl(err)
		return err
	}

	criuType := CRIUType_CRIU_RSYNC.Enum()
	if !c.live {
		criuType = nil
	}

	resp := MigrationHeader{Fs: MigrationFSType_RSYNC.Enum(), Criu: criuType}
	if err := c.send(&resp); err != nil {
		c.sendControl(err)
		return err
	}

	restore := make(chan error)
	go func(c *migrationSink) {
		imagesDir := ""
		srcIdmap := new(shared.IdmapSet)
		dstIdmap := c.container.IdmapSet()
		if dstIdmap == nil {
			dstIdmap = new(shared.IdmapSet)
		}

		if c.live {
			var err error
			imagesDir, err = ioutil.TempDir("", "lxd_migration_")
			if err != nil {
				os.RemoveAll(imagesDir)
				c.sendControl(err)
				return
			}

			defer func() {
				err := CollectCRIULogFile(c.container, imagesDir, "migration", "restore")
				/*
				 * If the checkpoint fails, we won't have any log to collect,
				 * so don't warn about that.
				 */
				if err != nil && !os.IsNotExist(err) {
					shared.Debugf("Error collectiong migration log file %s", err)
				}

				os.RemoveAll(imagesDir)
			}()

			if err := RsyncRecv(shared.AddSlash(imagesDir), c.criuConn); err != nil {
				restore <- err
				os.RemoveAll(imagesDir)
				c.sendControl(err)
				return
			}

			/*
			 * For unprivileged containers we need to shift the
			 * perms on the images images so that they can be
			 * opened by the process after it is in its user
			 * namespace.
			 */
			if dstIdmap != nil {
				if err := dstIdmap.ShiftRootfs(imagesDir); err != nil {
					restore <- err
					os.RemoveAll(imagesDir)
					c.sendControl(err)
					return
				}
			}
		}

		fsDir := c.container.RootfsPath()
		if err := RsyncRecv(shared.AddSlash(fsDir), c.fsConn); err != nil {
			restore <- err
			c.sendControl(err)
			return
		}

		for _, idmap := range header.Idmap {
			e := shared.IdmapEntry{
				Isuid:    *idmap.Isuid,
				Isgid:    *idmap.Isgid,
				Nsid:     int(*idmap.Nsid),
				Hostid:   int(*idmap.Hostid),
				Maprange: int(*idmap.Maprange)}
			srcIdmap.Idmap = shared.Extend(srcIdmap.Idmap, e)
		}

		if !reflect.DeepEqual(srcIdmap, dstIdmap) {
			if err := srcIdmap.UnshiftRootfs(shared.VarPath("containers", c.container.Name())); err != nil {
				restore <- err
				c.sendControl(err)
				return
			}

			if err := dstIdmap.ShiftRootfs(shared.VarPath("containers", c.container.Name())); err != nil {
				restore <- err
				c.sendControl(err)
				return
			}
		}

		if c.live {
			err := c.container.StartFromMigration(imagesDir)
			if err != nil {
				log := GetCRIULogErrors(imagesDir, "restore")
				err = fmt.Errorf("restore failed:\n%s", log)
			}

			restore <- err
		} else {
			restore <- nil
		}
	}(c)

	source := c.controlChannel()

	for {
		select {
		case err = <-restore:
			c.sendControl(err)
			return err
		case msg, ok := <-source:
			if !ok {
				c.disconnect()
				return fmt.Errorf("Got error reading source")
			}
			if !*msg.Success {
				c.disconnect()
				return fmt.Errorf(*msg.Message)
			} else {
				// The source can only tell us it failed (e.g. if
				// checkpointing failed). We have to tell the source
				// whether or not the restore was successful.
				shared.Debugf("Unknown message %v from source", msg)
			}
		}
	}
}

/*
 * Similar to forkstart, this is called when lxd is invoked as:
 *
 *    lxd forkmigrate <container> <lxcpath> <path_to_config> <path_to_criu_images>
 *
 * liblxc's restore() sets up the processes in such a way that the monitor ends
 * up being a child of the process that calls it, in our case lxd. However, we
 * really want the monitor to be daemonized, so we fork again. Additionally, we
 * want to fork for the same reasons we do forkstart (i.e. reduced memory
 * footprint when we fork tasks that will never free golang's memory, etc.)
 */
func MigrateContainer(args []string) error {
	if len(args) != 5 {
		return fmt.Errorf("Bad arguments %q", args)
	}

	name := args[1]
	lxcpath := args[2]
	configPath := args[3]
	imagesDir := args[4]

	defer os.Remove(configPath)

	c, err := lxc.NewContainer(name, lxcpath)
	if err != nil {
		return err
	}

	if err := c.LoadConfigFile(configPath); err != nil {
		return err
	}

	return c.Restore(lxc.RestoreOptions{
		Directory: imagesDir,
		Verbose:   true,
	})
}
