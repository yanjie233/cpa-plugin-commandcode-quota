package main

import "gopkg.in/yaml.v3"

// yamlUnmarshal 解析插件自有配置。宿主传进来的 config_yaml 是
// plugins.configs.<id> 下的子树。
func yamlUnmarshal(raw []byte, out any) error {
	return yaml.Unmarshal(raw, out)
}
