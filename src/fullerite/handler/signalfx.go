package handler

import (
	"fullerite/metric"

	"bytes"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/golang/protobuf/proto"
)

// SignalFx Handler
type SignalFx struct {
	BaseHandler
	endpoint  string
	authToken string
}

// NewSignalFx returns a new SignalFx handler.
func NewSignalFx() *SignalFx {
	s := new(SignalFx)
	s.name = "SignalFx"
	s.interval = DefaultInterval
	s.maxBufferSize = DefaultBufferSize
	s.timeout = time.Duration(DefaultTimeoutSec * time.Second)
	s.log = logrus.WithFields(logrus.Fields{"app": "fullerite", "pkg": "handler", "handler": "SignalFx"})
	s.channel = make(chan metric.Metric)
	s.emissionTimes = make([]float64, 0)
	return s
}

// Configure accepts the different configuration options for the signalfx handler
func (s *SignalFx) Configure(configMap map[string]interface{}) {
	if authToken, exists := configMap["authToken"]; exists == true {
		s.authToken = authToken.(string)
	} else {
		s.log.Error("There was no auth key specified for the SignalFx Handler, there won't be any emissions")
	}
	if endpoint, exists := configMap["endpoint"]; exists == true {
		s.endpoint = endpoint.(string)
	} else {
		s.log.Error("There was no endpoint specified for the SignalFx Handler, there won't be any emissions")
	}

	s.configureCommonParams(configMap)
}

// Endpoint returns SignalFx' API endpoint
func (s *SignalFx) Endpoint() string {
	return s.endpoint
}

// Run send metrics in the channel to SignalFx.
func (s *SignalFx) Run() {
	datapoints := make([]*DataPoint, 0, s.maxBufferSize)

	lastEmission := time.Now()
	lastHandlerMetricsEmission := lastEmission
	for incomingMetric := range s.Channel() {
		datapoint := s.convertToProto(incomingMetric)
		s.log.Debug("SignalFx datapoint: ", datapoint)
		datapoints = append(datapoints, datapoint)

		emitIntervalPassed := time.Since(lastEmission).Seconds() >= float64(s.interval)
		emitHandlerIntervalPassed := time.Since(lastHandlerMetricsEmission).Seconds() >= float64(s.interval)
		bufferSizeLimitReached := len(datapoints) >= s.maxBufferSize
		doEmit := emitIntervalPassed || bufferSizeLimitReached

		if emitHandlerIntervalPassed {
			lastHandlerMetricsEmission = time.Now()

			// Report HandlerEmitTiming
			m := s.makeEmissionTimeMetric()
			s.resetEmissionTimes()
			datapoints = append(datapoints, s.convertToProto(m))

			// Report setrics sent
			metricsSent := s.makeMetricsSentMetric()
			s.resetMetricsSent()
			datapoints = append(datapoints, s.convertToProto(metricsSent))

			// Report dropped metrics
			metricsDropped := s.makeMetricsDroppedMetric()
			s.resetMetricsDropped()
			datapoints = append(datapoints, s.convertToProto(metricsDropped))
		}

		if doEmit {
			// emit datapoints
			beforeEmission := time.Now()
			s.emitMetrics(datapoints)
			lastEmission = time.Now()

			emissionTimeInSeconds := lastEmission.Sub(beforeEmission).Seconds()
			s.log.Info("POST to SignalFx took ", emissionTimeInSeconds, " seconds")
			s.emissionTimes = append(s.emissionTimes, emissionTimeInSeconds)

			// reset datapoints
			datapoints = make([]*DataPoint, 0, s.maxBufferSize)
		}
	}
}

func (s *SignalFx) convertToProto(incomingMetric metric.Metric) *DataPoint {
	// Create a new values for the Datapoint that requires pointers.
	outname := s.Prefix() + incomingMetric.Name
	value := incomingMetric.Value

	datapoint := new(DataPoint)
	datapoint.Metric = &outname
	datapoint.Value = &Datum{
		DoubleValue: &value,
	}
	datapoint.Source = new(string)
	*datapoint.Source = "fullerite"

	switch incomingMetric.MetricType {
	case metric.Gauge:
		datapoint.MetricType = MetricType_GAUGE.Enum()
	case metric.Counter:
		datapoint.MetricType = MetricType_COUNTER.Enum()
	case metric.CumulativeCounter:
		datapoint.MetricType = MetricType_CUMULATIVE_COUNTER.Enum()
	}

	dimensions := incomingMetric.GetDimensions(s.DefaultDimensions())
	for key, value := range dimensions {
		// Dimension (protobuf) require a pointer to string
		// values. We need to create new string objects in the
		// scope of this for loop not to repeatedly add the
		// same key:value pairs to the the datapoint.
		dimensionKey := key
		dimensionValue := value
		dim := Dimension{
			Key:   &dimensionKey,
			Value: &dimensionValue,
		}
		datapoint.Dimensions = append(datapoint.Dimensions, &dim)
	}

	return datapoint
}

func (s *SignalFx) emitMetrics(datapoints []*DataPoint) {
	s.log.Info("Starting to emit ", len(datapoints), " datapoints")

	if len(datapoints) == 0 {
		s.log.Warn("Skipping send because of an empty payload")
		return
	}

	payload := new(DataPointUploadMessage)
	payload.Datapoints = datapoints

	if s.authToken == "" || s.endpoint == "" {
		s.log.Warn("Skipping emission because we're missing the auth token ",
			"or the endpoint, payload would have been ", payload)
		return
	}
	serialized, err := proto.Marshal(payload)
	if err != nil {
		s.log.Error("Failed to serailize payload ", payload)
		return
	}

	req, err := http.NewRequest("POST", s.endpoint, bytes.NewBuffer(serialized))
	if err != nil {
		s.log.Error("Failed to create a request to endpoint ", s.endpoint)
		return
	}
	req.Header.Set("X-SF-TOKEN", s.authToken)
	req.Header.Set("Content-Type", "application/x-protobuf")

	transport := http.Transport{
		Dial: s.dialTimeout,
	}
	client := &http.Client{
		Transport: &transport,
	}
	rsp, err := client.Do(req)
	if err != nil {
		s.log.Error("Failed to complete POST ", err)
		return
	}

	defer rsp.Body.Close()
	if rsp.Status != "200 OK" {
		body, _ := ioutil.ReadAll(rsp.Body)
		s.log.Error("Failed to post to signalfx @", s.endpoint,
			" status was ", rsp.Status,
			" rsp body was ", string(body),
			" payload was ", payload)
		return
	}

	s.log.Info("Successfully sent ", len(datapoints), " datapoints to SignalFx")
}

func (s *SignalFx) dialTimeout(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, s.timeout)
}
