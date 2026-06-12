//go:build windows

package main

// Windows has no O_NOFOLLOW; symlink creation needs elevation there and the
// Lstat check in saveToWorkspace still rejects existing symlinks, so the
// remaining race window is accepted on this platform.
const oNoFollow = 0
