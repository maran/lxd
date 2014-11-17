test_commits_signed_off() {
  # Don't run this test if we're not in travis; we don't want to muck with
  # people's local repos.
  if [ -z "$TRAVIS_PULL_REQUEST" ]; then
    return
  fi

  git remote add lxc https://github.com/lxc/lxd
  git fetch --depth=1 lxc master
  for i in $(git cherry lxc/master | grep '^+' | cut -d' ' -f2); do
    git show "$i" | grep 'Signed-off-by'
  done
}
