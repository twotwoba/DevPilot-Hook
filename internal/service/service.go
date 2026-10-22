// Package service 管理三平台的开机自启。
package service

// serviceArgs 构造后台服务启动参数。
func serviceArgs(dir string) []string {
	args := []string{"serve", "--foreground"}
	if dir != "" {
		args = append(args, "--dir", dir)
	}
	return args
}
