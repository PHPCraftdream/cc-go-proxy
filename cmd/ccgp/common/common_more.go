package common

import (
	"errors"
	"io/fs"
	"os"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	_ "golang.org/x/crypto/x509roots/fallback"
)

// angelServeArgs builds the argv of the detached background server.
func AngelServeArgs(serveArgs []string) []string {
	return append([]string{"--serve", "-s"}, serveArgs...)
}

func MissingDefaultConfig() bool {
	if os.Getenv(config.ConfigFileEnv) != "" {
		return false
	}
	p, err := config.DefaultConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return errors.Is(err, fs.ErrNotExist)
}
