//go:build !darwin && !linux

package service

import (
	"fmt"
	"runtime"
)

// Path 在不支持的平台上返回空。
func Path() string { return "" }

// Install 在不支持的平台上报错。
func Install(_, _ string) error {
	return fmt.Errorf("service: 当前平台 %s 暂不支持开机自启", runtime.GOOS)
}

// Uninstall 在不支持的平台上报错。
func Uninstall() error {
	return fmt.Errorf("service: 当前平台 %s 暂不支持开机自启", runtime.GOOS)
}

// Status 在不支持的平台上报错。
func Status() (string, error) {
	return "", fmt.Errorf("service: 当前平台 %s 暂不支持开机自启", runtime.GOOS)
}

// 其他平台不生成启动参数，保留引用以免被判定为未使用。
var _ = serviceArgs
