package processor

import "errors"

// ErrNoProcessor is returned by Processor.RunPass when no ResourceProcessor
// is registered for the requested resource. Callers (sync.Service) can
// distinguish this from genuine processor failures and log it less loudly.
var ErrNoProcessor = errors.New("processor: no processor registered for resource")
