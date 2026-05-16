package runtime

import "errors"

// ErrUnsupported indicates that a runtime method is not supported by the
// current backend. Callers can check with errors.Is to surface a clear
// message instead of treating a silent zero value as a feature.
var ErrUnsupported = errors.New("operation not supported by this runtime")
