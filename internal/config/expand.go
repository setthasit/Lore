package config

import (
	"strconv"

	"github.com/setthasit/Lore/internal/envx"
)

type stringField struct {
	name  string
	value *string
}

func (c *Config) expand() error {
	for _, field := range c.stringFields() {
		expanded, err := envx.Expand(field.name, *field.value)
		if err != nil {
			return err
		}
		*field.value = expanded
	}
	return nil
}

func (c *Config) stringFields() []stringField {
	fields := []stringField{
		{"workspace", &c.Workspace},
		{"index_path", &c.IndexPath},
	}
	for i := range c.Plugins {
		at := indexed("plugins", i)
		fields = append(fields,
			stringField{at + ".name", &c.Plugins[i].Name},
			stringField{at + ".from", &c.Plugins[i].From},
			stringField{at + ".pubkey", &c.Plugins[i].PubKey},
		)
	}
	fields = append(fields, instanceFields("sources", c.Sources)...)
	fields = append(fields, instanceFields("providers", c.Providers)...)
	fields = append(fields, roleBindingFields("embedder", &c.Embedder)...)
	if c.LLM != nil {
		fields = append(fields, roleBindingFields("llm", c.LLM)...)
	}
	for i := range c.Repos {
		at := indexed("repos", i)
		fields = append(fields,
			stringField{at + ".path", &c.Repos[i].Path},
			stringField{at + ".use", &c.Repos[i].Use},
			stringField{at + ".remote", &c.Repos[i].Remote},
		)
	}
	fields = append(fields,
		stringField{"server.http_addr", &c.Server.HTTPAddr},
		stringField{"server.grpc_addr", &c.Server.GRPCAddr},
	)
	if mtls := c.Server.MTLS; mtls != nil {
		fields = append(fields,
			stringField{"server.mtls.cert", &mtls.Cert},
			stringField{"server.mtls.key", &mtls.Key},
			stringField{"server.mtls.client_ca", &mtls.ClientCA},
		)
	}
	return fields
}

func instanceFields(section string, instances []Instance) []stringField {
	var fields []stringField
	for i := range instances {
		at := indexed(section, i)
		// `with:` is left raw: the registry owns expanding a plugin's own settings.
		fields = append(fields,
			stringField{at + ".id", &instances[i].ID},
			stringField{at + ".use", &instances[i].Use},
		)
	}
	return fields
}

func roleBindingFields(role string, binding *RoleBinding) []stringField {
	return []stringField{
		{role + ".provider", &binding.Provider},
		{role + ".model", &binding.Model},
	}
}

func indexed(section string, i int) string {
	return section + "[" + strconv.Itoa(i) + "]"
}
