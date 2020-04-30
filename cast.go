package cast

import (
	"bytes"
	"context"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cep21/circuit/v3"
	"github.com/cep21/circuit/v3/closers/hystrix"

	"github.com/sirupsen/logrus"
)

const (
	defaultDumpBodyLimit int = 8192
)

// Cast provides a set of rules to its request.
type Cast struct {
	client             *http.Client
	baseURL            string
	header             http.Header
	basicAuth          *BasicAuth
	bearerToken        string
	cookies            []*http.Cookie
	retry              int
	stg                backoffStrategy
	beforeRequestHooks []BeforeRequestHook
	requestHooks       []RequestHook
	responseHooks      []responseHook
	retryHooks         []RetryHook
	dumpFlag           int
	httpClientTimeout  time.Duration
	logger             *logrus.Logger
	h                  circuit.Manager
	defaultCircuitName string
}

// New returns an instance of Cast
func New(sl ...Setter) (*Cast, error) {
	c := new(Cast)
	c.header = make(http.Header)
	c.beforeRequestHooks = defaultBeforeRequestHooks
	c.requestHooks = defaultRequestHooks
	c.responseHooks = defaultResponseHooks
	c.retryHooks = defaultRetryHooks
	c.dumpFlag = fStd
	c.httpClientTimeout = 10 * time.Second
	c.logger = logrus.New()
	c.logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02 15:04:05",
	})
	c.logger.SetReportCaller(true)
	c.logger.SetOutput(os.Stderr)
	c.logger.SetLevel(logrus.InfoLevel)

	configuration := hystrix.Factory{
		ConfigureOpener: hystrix.ConfigureOpener{
			ErrorThresholdPercentage: 70,
			RequestVolumeThreshold:   10,
			RollingDuration:          10 * time.Second,
			Now:                      time.Now,
			NumBuckets:               10,
		},
		ConfigureCloser: hystrix.ConfigureCloser{
			SleepWindow:                  10 * time.Second,
			HalfOpenAttempts:             1,
			RequiredConcurrentSuccessful: 1,
		},
	}

	c.h = circuit.Manager{
		DefaultCircuitProperties: []circuit.CommandPropertiesConstructor{configuration.Configure},
	}

	for _, s := range sl {
		if err := s(c); err != nil {
			return nil, err
		}
	}

	c.client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          2000,
			MaxIdleConnsPerHost:   2000,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: c.httpClientTimeout,
	}

	return c, nil
}

// NewRequest returns an instance of Request.
func (c *Cast) NewRequest() *Request {
	return NewRequest()
}

// Do initiates a request.
func (c *Cast) Do(ctx context.Context, request *Request) (*Response, error) {
	body, err := request.ReqBody()
	if err != nil {
		c.logger.WithError(err).Error("request.reqBody")
		return nil, err
	}

	for _, hook := range c.beforeRequestHooks {
		err = hook(c, request)
		if err != nil {
			return nil, err
		}
	}

	request.rawRequest, err = http.NewRequestWithContext(ctx, request.method, c.baseURL+request.path, bytes.NewReader(body))
	if err != nil {
		c.logger.WithError(err).Error("http.NewRequest")
		return nil, err
	}

	for _, hook := range c.requestHooks {
		err = hook(c, request)
		if err != nil {
			return nil, err
		}
	}

	rep, err := c.genReply(request)
	if err != nil {
		return nil, err
	}

	for _, hook := range c.responseHooks {
		if err := hook(c, rep); err != nil {
			c.logger.WithError(err).Error("hook(c, resp)")
			return nil, err
		}
	}

	return rep, nil
}

func (c *Cast) genReply(request *Request) (*Response, error) {
	var (
		count = 0
		err   error
		resp  *Response
	)

	for {
		if count > c.retry {
			break
		}
		var (
			rawResponse *http.Response
			cb          *circuit.Circuit
		)
		if len(request.circuitName) > 0 {
			cb = c.h.GetCircuit(request.circuitName)
		} else {
			cb = c.h.GetCircuit(c.defaultCircuitName)
		}
		if count >= 1 {
			var body []byte
			body, err = request.ReqBody()
			if err != nil {
				return nil, err
			}
			request.rawRequest.Body = ioutil.NopCloser(bytes.NewReader(body))
		}
		var fallback bool
		if cb != nil {
			err = cb.Execute(context.TODO(), func(i context.Context) error {
				rawResponse, err = c.client.Do(request.rawRequest)
				if err != nil {
					fallback = true
					return err
				}
				return nil
			}, func(i context.Context, e error) error {
				return e
			})
		} else {
			rawResponse, err = c.client.Do(request.rawRequest)
		}
		count++
		request.prof.requestDone = time.Now().In(time.UTC)
		request.prof.requestCost = request.prof.requestDone.Sub(request.prof.requestStart)
		request.prof.receivingDone = time.Now().In(time.UTC)
		request.prof.receivingCost = request.prof.receivingDone.Sub(request.prof.receivingSart)

		resp = new(Response)
		resp.request = request
		resp.rawResponse = rawResponse
		if rawResponse != nil {
			var repBody []byte
			repBody, err = ioutil.ReadAll(rawResponse.Body)
			if err != nil {
				c.logger.WithError(err).Error("ioutil.ReadAll(rawResponse.Body)")
				return nil, err
			}
			err = rawResponse.Body.Close()
			if err != nil {
				c.logger.WithError(err).Error("rawResponse.Body.Close()")
				return nil, err
			}
			resp.body = repBody
			resp.statusCode = rawResponse.StatusCode
		}

		if fallback && cb.IsOpen() {
			break
		}

		var isRetry bool
		for _, hook := range c.retryHooks {
			if hook(resp, err) {
				isRetry = true
				break
			}
		}

		if isRetry && count <= c.retry && c.stg != nil {
			<-time.After(c.stg.backoff(count))
			continue
		}

		break
	}

	if err != nil {
		c.logger.WithError(err).Error("c.client.Do")
		return nil, err
	}

	return resp, nil
}

func (c *Cast) BaseURL() string {
	return c.baseURL
}
