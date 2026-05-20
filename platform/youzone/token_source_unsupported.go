//go:build !darwin

package youzone

import (
	"context"
	"fmt"
	"runtime"
)

func runBuiltInTokenSource(_ context.Context, source string) (helperOutput, error) {
	switch source {
	case accessTokenSourceChrome:
		return helperOutput{}, fmt.Errorf("built-in Chrome token source is only supported on macOS, not %s", runtime.GOOS)
	default:
		return helperOutput{}, fmt.Errorf("unknown built-in token source %q", source)
	}
}
