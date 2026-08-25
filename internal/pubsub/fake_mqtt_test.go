package pubsub

import (
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeToken is a completed mqtt.Token — every fake operation in this test
// suite finishes synchronously, so Wait/WaitTimeout/Done always report
// "already done" and Error carries whatever the fake decided.
type fakeToken struct {
	err error
}

func (f *fakeToken) Wait() bool                     { return true }
func (f *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (f *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (f *fakeToken) Error() error { return f.err }

type fakePublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakeMQTTClient implements mqtt.Client entirely in-memory so client.go's
// and claims.go's logic that calls through the mqtt.Client interface can
// be tested without a real broker. It only supports what this package's
// tests actually exercise (Publish recording, Connect/Disconnect/
// Subscribe/Unsubscribe as no-ops); it is not a general-purpose MQTT fake.
type fakeMQTTClient struct {
	mu         sync.Mutex
	published  []fakePublish
	publishErr error
}

func (f *fakeMQTTClient) IsConnected() bool       { return true }
func (f *fakeMQTTClient) IsConnectionOpen() bool  { return true }
func (f *fakeMQTTClient) Connect() mqtt.Token     { return &fakeToken{} }
func (f *fakeMQTTClient) Disconnect(quiesce uint) {}

func (f *fakeMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var data []byte
	switch p := payload.(type) {
	case []byte:
		data = p
	case string:
		data = []byte(p)
	}
	f.mu.Lock()
	f.published = append(f.published, fakePublish{topic: topic, qos: qos, retained: retained, payload: data})
	f.mu.Unlock()
	return &fakeToken{err: f.publishErr}
}

func (f *fakeMQTTClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	return &fakeToken{}
}

func (f *fakeMQTTClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	return &fakeToken{}
}

func (f *fakeMQTTClient) Unsubscribe(topics ...string) mqtt.Token             { return &fakeToken{} }
func (f *fakeMQTTClient) AddRoute(topic string, callback mqtt.MessageHandler) {}
func (f *fakeMQTTClient) OptionsReader() mqtt.ClientOptionsReader             { return mqtt.ClientOptionsReader{} }

func (f *fakeMQTTClient) lastPublishTo(topic string) (fakePublish, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.published) - 1; i >= 0; i-- {
		if f.published[i].topic == topic {
			return f.published[i], true
		}
	}
	return fakePublish{}, false
}

func (f *fakeMQTTClient) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

// fakeClaimLookup implements claimLookup for testing ClaimSandbox's
// decision logic without a real broker round-trip.
type fakeClaimLookup struct {
	payload []byte
	exists  bool
	err     error
}

func (f *fakeClaimLookup) currentRetained(topic string) ([]byte, bool, error) {
	return f.payload, f.exists, f.err
}
