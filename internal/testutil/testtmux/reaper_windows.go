//go:build windows

package testtmux

type ownerReaper struct{}

func startOwnerReaper(string) (*ownerReaper, error) {
	return &ownerReaper{}, nil
}

func ownerReaperExitCode() (int, bool) {
	return 0, false
}

func (*ownerReaper) stop() error {
	return nil
}

func (*ownerReaper) cancel() error {
	return nil
}
