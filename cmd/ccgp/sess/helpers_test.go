package sess

import (
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
