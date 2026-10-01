package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// v8OnlyRoots are the top-level sections that exist only in CLIProxyAPI's v8
// configuration layout, which re-homes the flat keys under grouped sections
// (source: CLIProxyAPI internal/config/config_v8.go, buildV8Paths).
var v8OnlyRoots = []string{
	"config-version",
	"server",
	"management",
	"access",
	"credentials",
	"requests",
	"oauth",
	"multimedia",
	"observability",
}

// v8OnlyRoutingKeys are the keys the v8 layout adds under the routing section
// that the flat layout shares with it.
var v8OnlyRoutingKeys = []string{"force-model-prefix", "retry", "cooldown"}

// rejectV8Layout refuses any setting written in CLIProxyAPI's v8 layout.
// LLMGW validates the security-sensitive settings by their flat spelling, and
// the SDK accepts both: a v8 spelling would reach the SDK unvalidated, and a
// document mixing both makes the SDK rewrite the file at load.
func rejectV8Layout(data []byte) error {
	var root map[string]yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse configuration layout:\n%w", err)
	}
	for _, key := range v8OnlyRoots {
		if _, found := root[key]; found {
			return v8LayoutError(key)
		}
	}
	if keys, found := root["api-keys"]; found && resolveAlias(keys).Kind == yaml.MappingNode {
		return v8LayoutError("api-keys (as a mapping)")
	}
	return rejectV8RoutingKeys(root["routing"])
}

// rejectV8RoutingKeys refuses the v8-only keys of the shared routing section.
func rejectV8RoutingKeys(routing yaml.Node) error {
	routing = resolveAlias(routing)
	if routing.Kind != yaml.MappingNode {
		return nil
	}
	var keys map[string]yaml.Node
	if err := routing.Decode(&keys); err != nil {
		return fmt.Errorf("parse configuration layout:\n%w", err)
	}
	for _, key := range v8OnlyRoutingKeys {
		if _, found := keys[key]; found {
			return v8LayoutError("routing." + key)
		}
	}
	return nil
}

// resolveAlias follows YAML aliases, through any chain of them, to the node
// they stand for. yaml.v3 keeps an alias as an unresolved node whose kind is
// not that of its target, so a kind check on the raw node would let a section
// hidden behind an anchor reference through.
func resolveAlias(node yaml.Node) yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = *node.Alias
	}
	return node
}

// v8LayoutError names the offending setting and the layout LLMGW accepts.
func v8LayoutError(key string) error {
	return fmt.Errorf(
		"validate configuration layout:\n%s belongs to the CLIProxyAPI v8 layout; LLMGW accepts only the flat layout",
		key,
	)
}
