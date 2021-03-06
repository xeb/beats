package elasticsearch

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/outputs/mode"
)

// Metrics that can retrieved through the expvar web interface.
var (
	ackedEvents            = expvar.NewInt("libbeatEsPublishedAndAckedEvents")
	eventsNotAcked         = expvar.NewInt("libbeatEsPublishedButNotAckedEvents")
	publishEventsCallCount = expvar.NewInt("libbeatEsPublishEventsCallCount")
)

type Client struct {
	Connection
	index string

	json jsonReader
}

type Connection struct {
	URL      string
	Username string
	Password string

	http      *http.Client
	connected bool
}

var (
	nameItems  = []byte("items")
	nameStatus = []byte("status")
	nameError  = []byte("error")
)

var (
	errExpectedItemObject    = errors.New("expected item response object")
	errExpectedStatusCode    = errors.New("expected item status code")
	errUnexpectedEmptyObject = errors.New("empty object")
	errExcpectedObjectEnd    = errors.New("expected end of object")
)

func NewClient(
	esURL, index string, proxyURL *url.URL, tls *tls.Config,
	username, password string,
) *Client {
	proxy := http.ProxyFromEnvironment
	if proxyURL != nil {
		proxy = http.ProxyURL(proxyURL)
	}

	client := &Client{
		Connection: Connection{
			URL:      esURL,
			Username: username,
			Password: password,
			http: &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: tls,
					Proxy:           proxy,
				},
			},
		},
		index: index,
	}
	return client
}

func (client *Client) Clone() *Client {
	newClient := &Client{
		Connection: Connection{
			URL:      client.URL,
			Username: client.Username,
			Password: client.Password,
			http: &http.Client{
				Transport: client.http.Transport,
			},
			connected: false,
		},
		index: client.index,
	}
	return newClient
}

// PublishEvents sends all events to elasticsearch. On error a slice with all
// events not published or confirmed to be processed by elasticsearch will be
// returned. The input slice backing memory will be reused by return the value.
func (client *Client) PublishEvents(
	events []common.MapStr,
) ([]common.MapStr, error) {
	publishEventsCallCount.Add(1)

	if !client.connected {
		return events, ErrNotConnected
	}

	// new request to store all events into
	request, err := client.startBulkRequest("", "", nil)
	if err != nil {
		logp.Err("Failed to perform any bulk index operations: %s", err)
		return events, err
	}

	// encode events into bulk request buffer, dropping failed elements from
	// events slice
	events = bulkEncodePublishRequest(request, client.index, events)
	if len(events) == 0 {
		return nil, nil
	}

	// send bulk request
	_, res, err := request.Flush()
	if err != nil {
		logp.Err("Failed to perform any bulk index operations: %s", err)
		return events, err
	}

	// check response for transient errors
	client.json.init(res.raw)
	failed_events := bulkCollectPublishFails(&client.json, events)
	ackedEvents.Add(int64(len(events) - len(failed_events)))
	eventsNotAcked.Add(int64(len(failed_events)))
	if len(failed_events) > 0 {
		return failed_events, mode.ErrTempBulkFailure
	}

	return nil, nil
}

// fillBulkRequest encodes all bulk requests and returns slice of events
// successfully added to bulk request.
func bulkEncodePublishRequest(
	requ *bulkRequest,
	index string,
	events []common.MapStr,
) []common.MapStr {
	okEvents := events[:0]
	for _, event := range events {
		meta := eventBulkMeta(index, event)
		err := requ.Send(meta, event)
		if err != nil {
			logp.Err("Failed to encode event: %s", err)
			continue
		}

		okEvents = append(okEvents, event)
	}
	return okEvents
}

func eventBulkMeta(index string, event common.MapStr) bulkMeta {
	ts := time.Time(event["@timestamp"].(common.Time)).UTC()
	index = fmt.Sprintf("%s-%d.%02d.%02d", index,
		ts.Year(), ts.Month(), ts.Day())
	meta := bulkMeta{
		Index: bulkMetaIndex{
			Index:   index,
			DocType: event["type"].(string),
		},
	}
	return meta
}

// bulkCollectPublishFails checks per item errors returning all events
// to be tried again due to error code returned for that items. If indexing an
// event failed due to some error in the event itself (e.g. does not respect mapping),
// the event will be dropped.
func bulkCollectPublishFails(
	reader *jsonReader,
	events []common.MapStr,
) []common.MapStr {
	if err := reader.expectDict(); err != nil {
		logp.Err("Failed to parse bulk respose: expected JSON object")
		return nil
	}

	// find 'items' field in response
	for {
		kind, name, err := reader.nextFieldName()
		if err != nil {
			logp.Err("Failed to parse bulk response")
			return nil
		}

		if kind == dictEnd {
			logp.Err("Failed to parse bulk response: no 'items' field in response")
			return nil
		}

		// found items array -> continue
		if bytes.Equal(name, nameItems) {
			break
		}

		reader.ignoreNext()
	}

	// check items field is an array
	if err := reader.expectArray(); err != nil {
		logp.Err("Failed to parse bulk respose: expected items array")
		return nil
	}

	count := len(events)
	failed := events[:0]
	for i := 0; i < count; i++ {
		status, msg, err := itemStatus(reader)
		if err != nil {
			return nil
		}

		if status < 300 {
			continue // ok value
		}

		if status < 500 && status != 429 {
			// hard failure, don't collect
			logp.Warn("Can not index event (status=%v): %v", status, msg)
			continue
		}

		logp.Info("Bulk item insert failed (i=%v, status=%v): %v", i, status, msg)
		failed = append(failed, events[i])
	}

	return failed
}

func itemStatus(reader *jsonReader) (int, []byte, error) {
	// skip outer dictionary
	if err := reader.expectDict(); err != nil {
		return 0, nil, errExpectedItemObject
	}

	// find first field in outer dictionary (e.g. 'create')
	kind, _, err := reader.nextFieldName()
	if err != nil {
		logp.Err("Failed to parse bulk response item: %s", err)
		return 0, nil, err
	}
	if kind == dictEnd {
		err = errUnexpectedEmptyObject
		logp.Err("Failed to parse bulk response item: %s", err)
		return 0, nil, err
	}

	// parse actual item response code and error message
	status, msg, err := itemStatusInner(reader)

	// close dictionary. Expect outer dictionary to have only one element
	kind, _, err = reader.step()
	if err != nil {
		logp.Err("Failed to parse bulk response item: %s", err)
		return 0, nil, err
	}
	if kind != dictEnd {
		err = errExcpectedObjectEnd
		logp.Err("Failed to parse bulk response item: %s", err)
		return 0, nil, err
	}

	return status, msg, nil
}

func itemStatusInner(reader *jsonReader) (int, []byte, error) {
	if err := reader.expectDict(); err != nil {
		return 0, nil, errExpectedItemObject
	}

	status := -1
	var msg []byte
	for {
		kind, name, err := reader.nextFieldName()
		if err != nil {
			logp.Err("Failed to parse bulk response item: %s", err)
		}
		if kind == dictEnd {
			break
		}

		switch {
		case bytes.Equal(name, nameStatus): // name == "status"
			status, err = reader.nextInt()
			if err != nil {
				logp.Err("Failed to parse bulk reponse item: %s", err)
				return 0, nil, err
			}

		case bytes.Equal(name, nameError): // name == "error"
			msg, err = reader.ignoreNext() // collect raw string for "error" field
			if err != nil {
				return 0, nil, err
			}

		default: // ignore unknown fields
			reader.ignoreNext()
		}
	}

	if status < 0 {
		return 0, nil, errExpectedStatusCode
	}
	return status, msg, nil
}

func (client *Client) PublishEvent(event common.MapStr) error {
	if !client.connected {
		return ErrNotConnected
	}

	ts := time.Time(event["@timestamp"].(common.Time))
	index := fmt.Sprintf("%s-%d.%02d.%02d",
		client.index, ts.Year(), ts.Month(), ts.Day())
	logp.Debug("output_elasticsearch", "Publish event: %s", event)

	// insert the events one by one
	status, _, err := client.Index(index, event["type"].(string), "", nil, event)
	if err != nil {
		logp.Warn("Fail to insert a single event: %s", err)
		if err == ErrJSONEncodeFailed {
			// don't retry unencodable values
			return nil
		}
	}
	switch {
	case status == 0: // event was not send yet
		return nil
	case status >= 500 || status == 429: // server error, retry
		return err
	case status >= 300 && status < 500:
		// won't be able to index event in Elasticsearch => don't retry
		return nil
	}

	return nil
}

func (conn *Connection) Connect(timeout time.Duration) error {
	var err error
	conn.connected, err = conn.Ping(timeout)
	if err != nil {
		return err
	}
	if !conn.connected {
		return ErrNotConnected
	}
	return nil
}

func (conn *Connection) Ping(timeout time.Duration) (bool, error) {
	debug("ES Ping(url=%v, timeout=%v)", conn.URL, timeout)

	conn.http.Timeout = timeout
	status, _, err := conn.execRequest("HEAD", conn.URL, nil)
	if err != nil {
		debug("Ping request failed with: %v", err)
		return false, err
	}

	debug("Ping status code: %v", status)
	return status < 300, nil
}

func (conn *Connection) IsConnected() bool {
	return conn.connected
}

func (conn *Connection) Close() error {
	conn.connected = false
	return nil
}

func (conn *Connection) request(
	method, path string,
	params map[string]string,
	body interface{},
) (int, []byte, error) {
	url := makeURL(conn.URL, path, params)
	logp.Debug("elasticsearch", "%s %s %s", method, url, body)

	var obj []byte
	if body != nil {
		var err error
		obj, err = json.Marshal(body)
		if err != nil {
			return 0, nil, ErrJSONEncodeFailed
		}
	}

	return conn.execRequest(method, url, bytes.NewReader(obj))
}

func (conn *Connection) execRequest(
	method, url string,
	body io.Reader,
) (int, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		logp.Warn("Failed to create request", err)
		return 0, nil, err
	}

	req.Header.Add("Accept", "application/json")
	if conn.Username != "" || conn.Password != "" {
		req.SetBasicAuth(conn.Username, conn.Password)
	}

	resp, err := conn.http.Do(req)
	if err != nil {
		conn.connected = false
		return 0, nil, err
	}
	defer closing(resp.Body)

	status := resp.StatusCode
	if status >= 300 {
		conn.connected = false
		return status, nil, fmt.Errorf("%v", resp.Status)
	}

	obj, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		conn.connected = false
		return status, nil, err
	}
	return status, obj, nil
}

func closing(c io.Closer) {
	err := c.Close()
	if err != nil {
		logp.Warn("Close failed with: %v", err)
	}
}
