//go:build !windows

package sender

import (
	"errors"
	"os/exec"
)

func platformSupported() error {
	return errors.New("native sender requires Windows 11; Linux capture is not implemented")
}
func enumerateSources() ([]Source, error) { return nil, platformSupported() }
func validateCurrentSource(Source) error  { return platformSupported() }
func openBrowser(string) error            { return platformSupported() }

type processGroup struct{}

func newProcessGroup() (*processGroup, error) { return nil, platformSupported() }
func (*processGroup) start(*exec.Cmd) error   { return platformSupported() }
func (*processGroup) close()                  {}
