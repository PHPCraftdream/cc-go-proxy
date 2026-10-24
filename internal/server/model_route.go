package server

import (
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/gofiber/fiber/v2"
)

// modelRoute reports configured routing, not an upstream inference result.
func modelRoute(c *fiber.Ctx) error {
	requested := c.Query("requested")
	if requested == "" || len(requested) > 256 || strings.ContainsAny(requested, "\r\n\x00") {
		return c.SendStatus(fiber.StatusBadRequest)
	}
	cfg := requestConfig(c)
	c.Set("Cache-Control", "no-store")
	return c.JSON(fiber.Map{
		"kind":           "ccgp:model-route:v1",
		"requestedModel": requested,
		"backendModel":   converter.MapModel(requested, cfg),
	})
}
