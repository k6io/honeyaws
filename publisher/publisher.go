package publisher

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/honeycombio/honeyaws/options"
	"github.com/honeycombio/honeyaws/state"
	"github.com/honeycombio/honeytail/event"
	"github.com/honeycombio/libhoney-go"
	"github.com/honeycombio/urlshaper"
	"github.com/sirupsen/logrus"
)

const (
	AWSApplicationLoadBalancerFormat = "aws_alb"
	AWSElasticLoadBalancerFormat     = "aws_elb"
	AWSCloudFrontWebFormat           = "aws_cf_web"
)

var (
	// Example ELB log format (aws_elb):
	// 2017-07-31T20:30:57.975041Z spline_reticulation_lb 10.11.12.13:47882 10.3.47.87:8080 0.000021 0.010962 0.000016 200 200 766 17 "PUT https://api.simulation.io:443/reticulate/spline/1 HTTP/1.1" "libhoney-go/1.3.3" ECDHE-RSA-AES128-GCM-SHA256 TLSv1.2
	//
	// Example ALB log format (aws_elbv2):
	// http 2018-02-18T03:03:10.432026Z app/alb-test-2/ebd66bfd69677bfa 142.44.241.206:60000 172.31.21.134:80 0.001 0.001 0.000 200 200 70 248 "GET http://alb-test-2-265992175.us-east-1.elb.amazonaws.com:80/ HTTP/1.0" "https://getroot.sh survey" - - arn:aws:elasticloadbalancing:us-east-1:729997878290:targetgroup/ec2instances/3bf8bbb3ab2b6080 "Root=1-5a88eced-40876ce050d010360bfb23bd" "-" "-"
	//
	// Example CloudFront log format (aws_cf_web):
	// 2014-05-23 01:13:11 FRA2 182 192.0.2.10 GET d111111abcdef8.cloudfront.net /view/my/file.html 200 www.displaymyfiles.com Mozilla/4.0%20(compatible;%20MSIE%205.0b1;%20Mac_PowerPC) - zip=98101 RefreshHit MRVMF7KydIvxMWfJIglgwHQwZsbG2IhRJ07sn9AkKUFSHS9EXAMPLE== d111111abcdef8.cloudfront.net http - 0.001 - - - RefreshHit HTTP/1.1

	logFormat = []byte(fmt.Sprintf(
		`log_format %s '$timestamp $elb $client_authority $backend_authority $request_processing_time $backend_processing_time $response_processing_time $elb_status_code $backend_status_code $received_bytes $sent_bytes "$request" "$user_agent" $ssl_cipher $ssl_protocol';
log_format %s '$timestamp $x_edge_location $sc_bytes $c_ip $cs_method $cs_host $cs_uri_stem $sc_status $cs_referer $cs_user_agent $cs_uri_query $cs_cookie $x_edge_result_type $x_edge_request_id $x_host_header $cs_protocol $cs_bytes $time_taken $x_forwarded_for $ssl_protocol $ssl_cipher $x_edge_response_result_type $cs_protocol_version';
log_format %s '$type $response_time $elb $client_authority $backend_authority $request_processing_time $backend_processing_time $response_processing_time $elb_status_code $backend_status_code $received_bytes $sent_bytes "$request" "$user_agent" $ssl_cipher $ssl_protocol $target_group_arn "$trace_id" "$domain_name" "$chosen_cert_arn" $matched_rule_priority $timestamp';`,
		AWSElasticLoadBalancerFormat,
		AWSCloudFrontWebFormat,
		AWSApplicationLoadBalancerFormat,
	))
	formatFileName string
)

func init() {
	// Set up the log format file for parsing in the future.
	formatFile, err := ioutil.TempFile("", "honeytail_fmt_file")
	if err != nil {
		logrus.Fatal(err)
	}

	if _, err := formatFile.Write(logFormat); err != nil {
		logrus.Fatal(err)
	}

	if err := formatFile.Close(); err != nil {
		logrus.Fatal(err)
	}

	formatFileName = formatFile.Name()
}

type Publisher interface {
	// Publish accepts an io.Reader and scans it line-by-line, parses the
	// relevant event from each line (using EventParser), and sends to the
	// target (Honeycomb).
	Publish(f state.DownloadedObject) error
}

type EventParser interface {
	// ParseEvents runs in a background goroutine and parses the downloaded
	// object, sending the events parsed from it further down the pipeline
	// using the output channel. er
	ParseEvents(obj state.DownloadedObject, out chan<- event.Event) error

	// DynSample dynamically samples events, reading them from `eventsCh`
	// and sending them to `sampledCh`. Behavior is dependent on the
	// publisher implementation, e.g., some fields might matter more for
	// ELB than for CloudFront.
	DynSample(in <-chan event.Event, out chan<- event.Event)
}

// HoneycombPublisher implements Publisher and sends the entries provided to
// Honeycomb. Publisher allows us to have only one point of entry to sending
// events to Honeycomb (if desired), as well as isolate line parsing, sampling,
// and URL sub-parsing logic.
type HoneycombPublisher struct {
	state.Stater
	EventParser
	APIHost             string
	SampleRate          int
	FinishedObjects     chan string
	parsedCh, sampledCh chan event.Event
}

func NewHoneycombPublisher(opt *options.Options, stater state.Stater, eventParser EventParser) *HoneycombPublisher {
	hp := &HoneycombPublisher{
		Stater:          stater,
		EventParser:     eventParser,
		FinishedObjects: make(chan string),
	}

	hp.parsedCh = make(chan event.Event)
	hp.sampledCh = make(chan event.Event)

	go sendEventsToHoneycomb(hp.sampledCh, opt.EdgeMode, opt.GrafanaCloudID, opt.GrafanaCloudEndpoint, opt.GrafanaCloudAPIKey, opt.Environment)
	go hp.EventParser.DynSample(hp.parsedCh, hp.sampledCh)

	return hp
}

// dropNegativeTimes is a helper method to eliminate AWS setting certain fields
// such as backend_processing_time to -1 indicating a timeout or network error.
// Since Honeycomb handles sparse data fine, we just delete these fields when
// they're set to this.
func dropNegativeTimes(ev *event.Event) {
	timeFields := []string{
		"response_processing_time",
		"request_processing_time",
		"backend_processing_time",
		"time_taken", // CloudFront -- not documented as ever being set to -1, but check anyway
	}
	for _, f := range timeFields {
		var tFloat float64
		if t, present := ev.Data[f]; present {
			switch t.(type) {
			case int64:
				tFloat = float64(t.(int64))
			case float64:
				tFloat = t.(float64)
			}
			if tFloat < 0 {
				delete(ev.Data, f)
				ev.Data["error"] = f + " was -1 -- upstream server timed out, disconnected, or sent malformed response"
			}
		}
	}
}

// parse the included X-Amzn-Trace-Id header if it is present in an ALB access
// log - see
// https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-request-tracing.html
// for reference
func addTraceData(ev *event.Event, edgeMode bool) {
	// in the original access log field the header is called 'trace_id'
	amznTraceID, ok := ev.Data["trace_id"].(string)
	if !ok {
		return
	}
	fields := strings.Split(amznTraceID, ";")
	rootSpan := true
	for _, field := range fields {
		kv := strings.Split(field, "=")
		// something we don't expect
		if len(kv) != 2 {
			continue
		}
		key := kv[0]
		val := kv[1]
		switch key {
		case "Root":
			ev.Data["trace.trace_id"] = val
		case "Self":
			ev.Data["trace.span_id"] = val
			rootSpan = false
		case "Parent":
			// if we're running in "edge mode", ignore the parent id. We want
			// the load balancer to always be the root of the trace.
			if !edgeMode {
				ev.Data["trace.parent_id"] = val
				rootSpan = false
			}
		case "Sampled":
			ev.Data["sampled"] = val
		default:
			ev.Data[key] = val
		}
	}
	if rootSpan {
		ev.Data["trace.span_id"] = ev.Data["trace.trace_id"].(string)
	}

	var durationMs float64
	if endTime, ok := ev.Data["response_time"].(string); ok {
		tm, err := time.Parse(time.RFC3339Nano, endTime)
		if err == nil {
			duration := tm.Sub(ev.Timestamp)
			if duration > 0 {
				durationMs = float64(duration / time.Millisecond)
			}
		}
	}
	if durationMs == 0.0 {
		durationMs, _ = ev.Data["request_processing_time"].(float64)
	}
	if durationMs > 0.0 {
		ev.Data["duration_ms"] = durationMs
	}

	if elb, ok := ev.Data["elb"].(string); ok {
		ev.Data["service_name"] = elb
	}
	if requestPath, ok := ev.Data["request_path"].(string); ok {
		ev.Data["name"] = requestPath
	}

	// rename misleading trace header field in event
	delete(ev.Data, "trace_id")
	ev.Data["request.headers.x-amzn-trace-id"] = amznTraceID
}

type Streams struct {
	Streams []Stream `json:"streams"`
}

type Stream struct {
	Stream map[string]string `json:"stream"`
	Values []interface{}     `json:"values"`
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

func sendEventsToHoneycomb(in <-chan event.Event, edgeMode bool, grafanaID string, grafanaEndpoint string, grafanaAPIKey string, environment string) {
	shaper := requestShaper{&urlshaper.Parser{}}
	for ev := range in {
		shaper.Shape("request", &ev)
		dropNegativeTimes(&ev)
		//addTraceData(&ev, edgeMode)
		data, err := json.Marshal(ev.Data)
		if err != nil {
			logrus.Error(err)
		}

		jsonPayload := Streams{
			Streams: []Stream{
				Stream{
					Stream: map[string]string{"environment": environment, "service": "honeyaws", "aws_elb": fmt.Sprintf("%v", ev.Data["elb"])},
					Values: []interface{}{[]string{strconv.FormatInt(ev.Timestamp.UnixNano(), 10), string(data)}},
				},
			},
		}
		b, _ := json.Marshal(jsonPayload)
		req, err := http.NewRequest("POST", grafanaEndpoint, bytes.NewBuffer(b))
		if err != nil {
			logrus.Error(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("Authorization", "Basic "+basicAuth(grafanaID, grafanaAPIKey))
		client := &http.Client{}
		_, err = client.Do(req)
		if err != nil {
			logrus.Error(err)
		}
	}
}

func (hp *HoneycombPublisher) Publish(downloadedObj state.DownloadedObject) error {
	logrus.WithField("object", downloadedObj.Object).Debug("Parse events begin")

	if err := hp.EventParser.ParseEvents(downloadedObj, hp.parsedCh); err != nil {
		return err
	}

	logrus.WithField("object", downloadedObj.Object).Debug("Parse events end")

	// Clean up the downloaded object.
	// TODO: Should always be done?
	if err := os.Remove(downloadedObj.Filename); err != nil {
		return fmt.Errorf("Error cleaning up downloaded object %s: %s", downloadedObj.Filename, err)
	}

	return nil
}

// Close flushes outstanding sends
func (hp *HoneycombPublisher) Close() {
	libhoney.Close()
}
