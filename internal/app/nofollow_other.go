//go:build !linux

package app

// oNoFollow does not exist outside Linux; O_EXCL still refuses existing names.
const oNoFollow = 0
