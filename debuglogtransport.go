package gogithub

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"
)

type Zaptransport struct {
	Base   http.RoundTripper
	Logger *zap.Logger
}

func (z *Zaptransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if z.Logger == nil {
		return z.Base.RoundTrip(request)
	}
	var bodyReadAsBuffer bytes.Buffer
	if _, err := io.Copy(&bodyReadAsBuffer, request.Body); err != nil {
		return nil, fmt.Errorf("error reading request body: %w", err)
	}
	request.Body = io.NopCloser(&bodyReadAsBuffer)
	z.Logger.Debug("staring request", zap.String("url", request.URL.String()), zap.String("method", request.Method), zap.Any("header", request.Header), zap.Any("body", bodyReadAsBuffer.String()))
	defer z.Logger.Debug("ending request", zap.String("url", request.URL.String()))
	resp, err := z.Base.RoundTrip(request)
	if err != nil {
		z.Logger.Debug("response error", zap.Error(err))
	} else {
		z.Logger.Debug("response", zap.Any("header", resp.Header), zap.Any("body", resp.Body))
	}
	return resp, err
}

func DebugLogTransport(base http.RoundTripper, logger *zap.Logger) http.RoundTripper {
	if logger.Core().Enabled(zap.DebugLevel) {
		return &Zaptransport{
			Base:   base,
			Logger: logger,
		}
	}
	return base
}

var _ http.RoundTripper = &Zaptransport{}
