//go:build !linux

package container

// These stubs exist so the module compiles and the portable packages (spec
// validation, cgroup file logic, overlay option formatting) can be developed
// and tested on any machine. Everything here needs Linux namespaces, which
// have no equivalent elsewhere.

func platformCheck() error { return ErrUnsupportedPlatform }

// Run is unsupported off Linux.
func Run(Config) (*Result, error) { return nil, ErrUnsupportedPlatform }

// Init is unsupported off Linux and is never reached, since Run fails first.
func Init() { panic("container: Init is only callable on Linux") }
