package server

import (
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/creds"
	"github.com/gofiber/fiber/v2"
)

// callPrefix is the URL prefix that carries the access token: /call/{token}/...
const callPrefix = "/call/"

const localsCfg = "ccgp.cfg"

// profileBuilder returns the per-request config for a profile.
type profileBuilder func(profile string) (*config.Config, error)

// requireProfile resolves the :token path param to a profile and stores its
// config in Locals. Unknown token returns 404 so the endpoint is not revealed.
func requireProfile(index *creds.TokenIndex, build profileBuilder) fiber.Handler {
	return func(c *fiber.Ctx) error {
		profile, ok := index.Lookup(c.Params("token"))
		if !ok {
			return c.SendStatus(fiber.StatusNotFound)
		}
		cfg, err := build(profile)
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"type":  "error",
				"error": fiber.Map{"type": "api_error", "message": err.Error()},
			})
		}
		c.Locals(localsCfg, cfg)
		return c.Next()
	}
}

// requestConfig returns the config stored by requireProfile.
func requestConfig(c *fiber.Ctx) *config.Config {
	cfg, _ := c.Locals(localsCfg).(*config.Config)
	return cfg
}

// maskToken hides the token segment of /call/{token}/... paths for logging.
func maskToken(path string) string {
	if !strings.HasPrefix(path, callPrefix) {
		return path
	}
	rest := path[len(callPrefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return callPrefix + "***" + rest[i:]
	}
	return callPrefix + "***"
}
