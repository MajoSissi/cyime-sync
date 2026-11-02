//go:build !windows

package clipboard

// New 在非 Windows 平台返回不支持，保证 go vet / go test 可以跨平台跑通。
func New() (Clipboard, error) {
	return nil, ErrUnsupported
}
