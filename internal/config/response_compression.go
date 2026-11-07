package config

import (
	"errors"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// disableResponseCompression turns off the SDK's negotiated compression of
// client responses. LLMGW's response wrappers rewrite the plain bytes the SDK
// handlers write, and the compression layer sits beneath them: handed
// compressed bytes, the tool-name strip and the markup screen would find
// nothing to act on, and a stream would be held whole instead of relayed.
//
// It is disabled here rather than by removing the client's Accept-Encoding,
// because the SDK reads that header to recognise Claude Code clients. An
// operator who explicitly enables it is refused rather than overridden.
func disableResponseCompression(proxy *sdkconfig.Config) error {
	if enabled := proxy.ResponseCompression.Enabled; enabled != nil && *enabled {
		return errors.New(
			"validate configuration response rewriting:\nresponse-compression.enabled must be false",
		)
	}

	disabled := false
	proxy.ResponseCompression.Enabled = &disabled
	return nil
}
