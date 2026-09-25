package app

import "syscall"

// oNoFollow refuses to open a symbolic link as the final path component.
const oNoFollow = syscall.O_NOFOLLOW
