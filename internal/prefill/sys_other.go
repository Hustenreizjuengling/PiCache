//go:build !linux

package prefill

const oNoFollow = 0

func binaryReplaced() bool { return false }
