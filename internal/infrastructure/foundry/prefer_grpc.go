package foundry

import (
	"os"
	"strconv"
)

// preferGRPCFromEnv mirrors the A7.2 flag used across services. Defaults
// to true.
func preferGRPCFromEnv() bool {
	v := os.Getenv("APP_PREFER_GRPC")
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return b
}
