package config

import (
	"slices"
	"strconv"

	"github.com/setthasit/Lore/internal/envx"
)

type stringField struct {
	name  string
	value *string
}

func (c *Config) expand() error {
	return expandFields(c.stringFields())
}

type PluginRefs struct {
	Plugins   []PluginDecl
	Sources   []Instance
	Providers []Instance
	Repos     []RepoDecl
}

// ExpandPluginRefs expands, on a copy, plugins[] and the fields that name or label a plugin's users.
func (c *Config) ExpandPluginRefs() (PluginRefs, error) {
	refs := PluginRefs{
		Plugins:   slices.Clone(c.Plugins),
		Sources:   slices.Clone(c.Sources),
		Providers: slices.Clone(c.Providers),
		Repos:     slices.Clone(c.Repos),
	}
	if err := expandFields(refs.stringFields()); err != nil {
		return PluginRefs{}, err
	}
	return refs, nil
}

func (r *PluginRefs) stringFields() []stringField {
	var fields []stringField
	for i := range r.Plugins {
		fields = append(fields, pluginFields(i, &r.Plugins[i])...)
	}
	fields = append(fields, instanceFields("sources", r.Sources)...)
	fields = append(fields, instanceFields("providers", r.Providers)...)
	for i := range r.Repos {
		fields = append(fields, repoRefFields(i, &r.Repos[i])...)
	}
	return fields
}

func (r PluginRefs) InstancesUsing(plugin string) []string {
	return (&Config{Sources: r.Sources, Providers: r.Providers, Repos: r.Repos}).InstancesUsing(plugin)
}

func expandFields(fields []stringField) error {
	for _, field := range fields {
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
		fields = append(fields, pluginFields(i, &c.Plugins[i])...)
	}
	fields = append(fields, instanceFields("sources", c.Sources)...)
	fields = append(fields, instanceFields("providers", c.Providers)...)
	fields = append(fields, roleBindingFields("embedder", &c.Embedder)...)
	if c.LLM != nil {
		fields = append(fields, roleBindingFields("llm", c.LLM)...)
	}
	for i := range c.Repos {
		fields = append(fields, repoRefFields(i, &c.Repos[i])...)
		fields = append(fields, stringField{indexed("repos", i) + ".remote", &c.Repos[i].Remote})
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

func PluginField(index int, key string) string {
	return indexed("plugins", index) + "." + key
}

func pluginFields(index int, decl *PluginDecl) []stringField {
	return []stringField{
		{PluginField(index, "name"), &decl.Name},
		{PluginField(index, "from"), &decl.From},
		{PluginField(index, "pubkey"), &decl.PubKey},
	}
}

func repoRefFields(index int, repo *RepoDecl) []stringField {
	at := indexed("repos", index)
	return []stringField{
		{at + ".path", &repo.Path},
		{at + ".use", &repo.Use},
	}
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
