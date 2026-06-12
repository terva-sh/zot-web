//go:build unix

package main

import "syscall"

// oNoFollow makes the final open refuse to traverse a symlink, closing the
// TOCTOU between saveToWorkspace's Lstat check and the OpenFile.
const oNoFollow = syscall.O_NOFOLLOW
