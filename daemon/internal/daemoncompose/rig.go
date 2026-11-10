package daemoncompose

import (
	"errors"
	"fmt"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

type RigHoldOutput struct {
	Token    string                 `json:"token"`
	Manifest daemonlock.RigManifest `json:"manifest"`
}

func ReadRigToken(path string) (string, error) {
	acquisition, err := ReadRigAcquisition(path)
	if err != nil {
		return "", err
	}
	return acquisition.Token, nil
}

func ReadRigAcquisition(path string) (RigHoldOutput, error) {
	// #nosec G304 -- the operator supplies the private acquisition file emitted by rig hold.
	body, err := os.ReadFile(path)
	if err != nil {
		return RigHoldOutput{}, fmt.Errorf("read rig token file: %w", err)
	}
	var acquisition RigHoldOutput
	if err := strictjson.Decode(
		body, &acquisition, strictjson.RejectInvalidUTF8, strictjson.Limit(128<<10),
	); err != nil {
		return RigHoldOutput{}, fmt.Errorf("decode rig token file: %w", err)
	}
	if acquisition.Token == "" {
		return RigHoldOutput{}, errors.New("rig token file has no token")
	}
	return acquisition, nil
}
