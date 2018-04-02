package interceptor

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/getsentry/raven-go"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"net/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxStackSize = 4096
)

var (
	defaultBuckets = []float64{10, 20, 30, 50, 80, 100, 200, 300, 500, 1000, 2000, 3000}
)

type Monitor struct {
	sentryClient *raven.Client
	reqCounter   *prometheus.CounterVec
	errCounter   *prometheus.CounterVec
	respLatency  *prometheus.HistogramVec
}

// port 标记进程端口
func NewMonitor(application string, port int, sentryDSN string, buckets ...float64) (*Monitor, error) {
	var m Monitor
	client, err := raven.New(sentryDSN)
	if err != nil {
		fmt.Println("Monitor: init failed: ", err.Error())
		return nil, err
	}
	m.sentryClient = client

	process := strconv.Itoa(port)
	m.reqCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   application,
			Name:        "requests_total",
			Help:        "Total request counts",
			ConstLabels: prometheus.Labels{"method": "rpc", "process": process},
		},
		[]string{
			"endpoint",
		},
	)
	prometheus.MustRegister(m.reqCounter)

	m.errCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   application,
			Name:        "error_total",
			Help:        "Total error counts",
			ConstLabels: prometheus.Labels{"method": "rpc", "process": process},
		},
		[]string{
			"endpoint",
		},
	)
	prometheus.MustRegister(m.errCounter)

	if len(buckets) == 0 {
		buckets = defaultBuckets
	}
	m.respLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace:   application,
			Name:        "response_latency_millisecond",
			Help:        "Response latency (millisecond)",
			ConstLabels: prometheus.Labels{"method": "rpc", "process": process},
			Buckets:     buckets,
		},
		[]string{
			"endpoint",
		},
	)
	prometheus.MustRegister(m.respLatency)

	return &m, nil
}

func (m *Monitor) observe(method string, latency float64) {
	labels := prometheus.Labels{"endpoint": method}
	m.reqCounter.With(labels).Inc()
	m.respLatency.With(labels).Observe(latency)
}

func (m *Monitor) observeError(method string, err error) {
	labels := prometheus.Labels{"endpoint": method}
	m.errCounter.With(labels).Inc()

	m.sentryClient.CaptureError(err, nil)
}

func (m *Monitor) Monitoring(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	start := time.Now()
	resp, err = handler(ctx, req)
	if err == nil {
		m.observe(info.FullMethod, float64(time.Since(start).Nanoseconds())/1000000)
	} else {
		m.observeError(info.FullMethod, err)
	}

	return resp, err
}


func (m *Monitor) Recovery(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			// log stack
			stack := make([]byte, maxStackSize)
			stack = stack[:runtime.Stack(stack, false)]
			errStack := string(stack)
			fmt.Printf("panic grpc invoke: %s, err=%v, stack:\n%s", info.FullMethod, r, errStack)

			func() {
				defer func() {
					if r := recover(); err != nil {
						fmt.Printf("report sentry failed: %s, trace:\n%s", r, debug.Stack())
						err = grpc.Errorf(codes.Internal, "panic error: %v", r)
					}
				}()
				switch rVal := r.(type) {
				case error:
					m.observeError(info.FullMethod, rVal)
				default:
					m.observeError(info.FullMethod, errors.New(fmt.Sprint(rVal)))
				}
			}()

			// if panic, set custom error to 'err', in order that client and sense it.
			err = grpc.Errorf(codes.Internal, "panic error: %v", r)
		}
	}()

	return handler(ctx, req)
}


// (resp interface{}, err error)
func (m *Monitor) MonitoringMethod(ctx context.Context, method string, start time.Time) (err error) {
	// recovery func
	defer func() {
		if r := recover(); r != nil {
			// log stack
			stack := make([]byte, maxStackSize)
			stack = stack[:runtime.Stack(stack, false)]
			errStack := string(stack)
			fmt.Printf("panic method: %s, err=%v, stack:\n%s", method, r, errStack)

			func() {
				defer func() {
					if r := recover(); err != nil {
						fmt.Printf("report sentry failed: %s, trace:\n%s", r, debug.Stack())
						err = fmt.Errorf("recover fail, panic error: %v", r)
					}
				}()
				switch rVal := r.(type) {
				case error:
					m.observeError(method, rVal)
				default:
					m.observeError(method, errors.New(fmt.Sprint(rVal)))
				}
			}()

			// if panic, set custom error to 'err', in order that client and sense it.
			err = fmt.Errorf("recover fail, panic error: %v", r)
		}
	}()

	if err == nil {
		m.observe(method, float64(time.Since(start).Nanoseconds())/1000000)
	} else {
		m.observeError(method, err)
	}
	return err
}


func StartMetricsServer(metricsPort int) {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		fmt.Println(http.ListenAndServe(fmt.Sprintf(":%v", metricsPort), nil))
	}()
}