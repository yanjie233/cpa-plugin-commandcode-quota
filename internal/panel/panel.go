// Package panel 内嵌看板页面。UI 与 ccquota 保持一致,但不需要任何密码:
// 页面本身挂在宿主的管理鉴权之下,能打开就已经通过认证。
package panel

import _ "embed"

//go:embed panel.html
var html []byte

// HTML 返回看板页面。
func HTML() []byte { return html }
