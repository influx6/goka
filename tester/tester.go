package tester

import (
	"flag"
	"fmt"
	"hash"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"sync"

	"github.com/facebookgo/ensure"
	"github.com/golang/protobuf/proto"

	"github.com/lovoo/goka"
	"github.com/lovoo/goka/kafka"
	"github.com/lovoo/goka/storage"
)

// Codec decodes and encodes from and to []byte
type Codec interface {
	Encode(value interface{}) (data []byte, err error)
	Decode(data []byte) (value interface{}, err error)
}

var (
	debug  = flag.Bool("tester-debug", false, "show debug prints of the tester.")
	logger = log.New(ioutil.Discard, "<Tester> ", 0)
)

// EmitHandler abstracts a function that allows to overwrite kafkamock's Emit function to
// simulate producer errors
type EmitHandler func(topic string, key string, value []byte) *kafka.Promise

type queuedMessage struct {
	topic string
	key   string
	value []byte
}

// Tester allows interacting with a test processor
type Tester struct {
	t T

	producerMock *producerMock
	topicMgrMock *topicMgrMock
	emitHandler  EmitHandler
	storages     map[string][]storage.Storage

	codecs      map[string]goka.Codec
	topicQueues map[string]*queue
	mQueues     sync.RWMutex

	queuedMessages []*queuedMessage
}

func (km *Tester) queueForTopic(topic string) *queue {
	km.mQueues.RLock()
	defer km.mQueues.RUnlock()
	q, exists := km.topicQueues[topic]
	if !exists {
		panic(fmt.Errorf("No queue for topic %s", topic))
	}
	return q
}

// CreateMessageTracker creates a message tracker that starts tracking
// the messages from the end of the current queues
func (km *Tester) NewMessageTrackerFromEnd() *MessageTracker {
	km.waitStartup()

	mt := newMessageTracker(km, km.t)
	km.mQueues.RLock()
	defer km.mQueues.RUnlock()
	for topic := range km.topicQueues {
		mt.MoveToEnd(topic)
	}
	return mt
}

func (km *Tester) getOrCreateQueue(topic string) *queue {
	km.mQueues.RLock()
	_, exists := km.topicQueues[topic]
	km.mQueues.RUnlock()
	if !exists {
		km.mQueues.Lock()
		if _, exists = km.topicQueues[topic]; !exists {
			km.topicQueues[topic] = newQueue(topic)
		}
		km.mQueues.Unlock()
	}

	km.mQueues.RLock()
	defer km.mQueues.RUnlock()
	return km.topicQueues[topic]
}

// T abstracts the interface we assume from the test case.
// Will most likely be *testing.T
type T interface {
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
	Fatal(a ...interface{})
}

// New returns a new Tester.
// It should be passed as goka.WithTester to goka.NewProcessor.
func New(t T) *Tester {

	// activate the logger if debug is turned on
	if *debug {
		logger.SetOutput(os.Stdout)
	}

	tester := &Tester{
		t:           t,
		codecs:      make(map[string]goka.Codec),
		topicQueues: make(map[string]*queue),
		storages:    make(map[string][]storage.Storage),
	}
	tester.producerMock = newProducerMock(tester.handleEmit)
	tester.topicMgrMock = newTopicMgrMock(tester)
	return tester
}

func (km *Tester) registerCodec(topic string, codec goka.Codec) {
	if existingCodec, exists := km.codecs[topic]; exists {
		if reflect.TypeOf(codec) != reflect.TypeOf(existingCodec) {
			panic(fmt.Errorf("There are different codecs for the same topic. This is messed up (%#v, %#v)", codec, existingCodec))
		}
	}
	km.codecs[topic] = codec
}

func (km *Tester) codecForTopic(topic string) goka.Codec {
	codec, exists := km.codecs[topic]
	if !exists {
		panic(fmt.Errorf("No codec for topic %s registered.", topic))
	}
	return codec
}

// RegisterGroupGraph is called by a processor when the tester is passed via
// `WithTester(..)`.
// This will setup the tester with the neccessary consumer structure
func (km *Tester) RegisterGroupGraph(gg *goka.GroupGraph) {
	if gg.GroupTable() != nil {
		km.getOrCreateQueue(gg.GroupTable().Topic()).expectSimpleConsumer()
		km.registerCodec(gg.GroupTable().Topic(), gg.GroupTable().Codec())
	}

	for _, input := range gg.InputStreams() {
		km.getOrCreateQueue(input.Topic()).expectGroupConsumer()
		km.registerCodec(input.Topic(), input.Codec())
	}

	for _, output := range gg.OutputStreams() {
		km.registerCodec(output.Topic(), output.Codec())
	}
	for _, join := range gg.JointTables() {
		km.getOrCreateQueue(join.Topic()).expectSimpleConsumer()
		km.registerCodec(join.Topic(), join.Codec())
	}

	if loop := gg.LoopStream(); loop != nil {
		km.getOrCreateQueue(loop.Topic()).expectGroupConsumer()
		km.registerCodec(loop.Topic(), loop.Codec())
	}

	for _, lookup := range gg.LookupTables() {
		km.getOrCreateQueue(lookup.Topic()).expectSimpleConsumer()
		km.registerCodec(lookup.Topic(), lookup.Codec())
	}

}

// TopicManagerBuilder returns the topicmanager builder when this tester is used as an option
// to a processor
func (km *Tester) TopicManagerBuilder() kafka.TopicManagerBuilder {
	return func(brokers []string) (kafka.TopicManager, error) {
		return km.topicMgrMock, nil
	}
}

// ConsumerBuilder returns the consumer builder when this tester is used as an option
// to a processor
func (km *Tester) ConsumerBuilder() kafka.ConsumerBuilder {
	return func(b []string, group, clientID string) (kafka.Consumer, error) {
		return newConsumer(km), nil
	}
}

// ProducerBuilder returns the producer builder when this tester is used as an option
// to a processor
func (km *Tester) ProducerBuilder() kafka.ProducerBuilder {
	return func(b []string, cid string, hasher func() hash.Hash32) (kafka.Producer, error) {
		return km.producerMock, nil
	}
}

// StorageBuilder returns the storage builder when this tester is used as an option
// to a processor
func (km *Tester) StorageBuilder() storage.Builder {
	return func(topic string, partition int32) (storage.Storage, error) {
		st := storage.NewMemory()
		km.storages[topic] = append(km.storages[topic], st)
		return st, nil
	}
}

// ConsumeProto simulates a message on kafka in a topic with a key.
func (km *Tester) ConsumeProto(topic string, key string, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil && km.t != nil {
		km.t.Errorf("Error marshaling message for consume: %v", err)
	}
	km.waitStartup()
	km.pushMessage(topic, key, data)
	km.waitForConsumers()
}

// ConsumeString simulates a message with a string payload.
func (km *Tester) ConsumeString(topic string, key string, msg string) {
	km.waitStartup()
	km.pushMessage(topic, key, []byte(msg))
	km.waitForConsumers()
}

func (km *Tester) waitForConsumers() {

	logger.Printf("waiting for consumers")
	for {
		if len(km.queuedMessages) == 0 {
			break
		}
		next := km.queuedMessages[0]
		km.queuedMessages = km.queuedMessages[1:]

		km.getOrCreateQueue(next.topic).push(next.key, next.value)

		km.mQueues.RLock()
		for {
			var messagesConsumed int
			for _, queue := range km.topicQueues {
				messagesConsumed += queue.waitForConsumers()
			}
			if messagesConsumed == 0 {
				break
			}
		}
		km.mQueues.RUnlock()
	}

	logger.Printf("waiting for consumers done")
}

func (km *Tester) waitStartup() {
	logger.Printf("Tester: Waiting for startup")
	km.mQueues.RLock()
	defer km.mQueues.RUnlock()
	for _, queue := range km.topicQueues {
		queue.waitConsumersInit()
	}
	logger.Printf("Tester: Waiting for startup done")
}

// Consume a message using the topic's configured codec
func (km *Tester) Consume(topic string, key string, msg interface{}) {
	km.waitStartup()

	// if the user wants to send a nil for some reason,
	// just let her. Goka should handle it accordingly :)
	value := reflect.ValueOf(msg)
	if msg == nil || (value.Kind() == reflect.Ptr && value.IsNil()) {
		km.pushMessage(topic, key, nil)
	} else {
		data, err := km.codecForTopic(topic).Encode(msg)
		if err != nil {
			panic(fmt.Errorf("Error encoding value %v: %v", msg, err))
		}
		km.pushMessage(topic, key, data)
	}

	km.waitForConsumers()
}

// ConsumeData pushes a marshalled byte slice to a topic and a key
func (km *Tester) ConsumeData(topic string, key string, data []byte) {
	km.waitStartup()
	km.pushMessage(topic, key, data)
	km.waitForConsumers()
}

func (km *Tester) pushMessage(topic string, key string, data []byte) {
	km.queuedMessages = append(km.queuedMessages, &queuedMessage{topic: topic, key: key, value: data})
}

// handleEmit handles an Emit-call on the producerMock.
// This takes care of queueing calls
// to handled topics or putting the emitted messages in the emitted-messages-list
func (km *Tester) handleEmit(topic string, key string, value []byte) *kafka.Promise {
	promise := kafka.NewPromise()
	km.pushMessage(topic, key, value)
	return promise.Finish(nil)
}

// TableValue attempts to get a value from any table that is used in the kafka mock.
func (km *Tester) TableValue(table goka.Table, key string) interface{} {
	km.waitStartup()

	topic := string(table)
	sts := km.storages[topic]
	if len(sts) == 0 {
		panic(fmt.Errorf("topic %s does not exist", topic))
	}

	item, err := sts[0].Get(key)
	ensure.Nil(km.t, err)
	if item == nil {
		return nil
	}
	value, err := km.codecForTopic(topic).Decode(item)
	ensure.Nil(km.t, err)
	return value
}

// SetTableValue sets a value in a processor's or view's table direcly via storage
func (km *Tester) SetTableValue(table goka.Table, key string, value interface{}) {
	km.waitStartup()

	logger.Printf("setting value is not implemented yet.")

	topic := string(table)
	sts := km.storages[topic]
	if len(sts) == 0 {
		panic(fmt.Errorf("storage for topic %s does not exist", topic))
	}
	data, err := km.codecForTopic(topic).Encode(value)
	ensure.Nil(km.t, err)

	for _, st := range sts {
		err = st.Set(key, data)
		if err != nil {
			panic(fmt.Errorf("Error setting key %s in storage %s: %v", key, table, err))
		}
	}
}

// ReplaceEmitHandler replaces the emitter.
func (km *Tester) ReplaceEmitHandler(emitter EmitHandler) {
	km.producerMock.emitter = emitter
}

// ClearValues resets all table values
func (km *Tester) ClearValues() {
	for topic, sts := range km.storages {
		for _, st := range sts {
			logger.Printf("clearing all values from storage for topic %s", topic)
			it, _ := st.Iterator()
			for it.Next() {
				st.Delete(string(it.Key()))
			}
		}
	}
}

type topicMgrMock struct {
	tester *Tester
}

// EnsureTableExists checks that a table (log-compacted topic) exists, or create one if possible
func (tm *topicMgrMock) EnsureTableExists(topic string, npar int) error {
	return nil
}

// EnsureStreamExists checks that a stream topic exists, or create one if possible
func (tm *topicMgrMock) EnsureStreamExists(topic string, npar int) error {
	return nil
}

// Partitions returns the number of partitions of a topic, that are assigned to the running
// instance, i.e. it doesn't represent all partitions of a topic.
func (tm *topicMgrMock) Partitions(topic string) ([]int32, error) {
	return []int32{0}, nil
}

// Close closes the topic manager.
// No action required in the mock.
func (tm *topicMgrMock) Close() error {
	return nil
}

func newTopicMgrMock(tester *Tester) *topicMgrMock {
	return &topicMgrMock{
		tester: tester,
	}
}

type producerMock struct {
	emitter EmitHandler
}

func newProducerMock(emitter EmitHandler) *producerMock {
	return &producerMock{
		emitter: emitter,
	}
}

// Emit emits messages to arbitrary topics.
// The mock simply forwards the emit to the KafkaMock which takes care of queueing calls
// to handled topics or putting the emitted messages in the emitted-messages-list
func (p *producerMock) Emit(topic string, key string, value []byte) *kafka.Promise {
	return p.emitter(topic, key, value)
}

// Close closes the producer mock
// No action required in the mock.
func (p *producerMock) Close() error {
	fmt.Println("Closing producer mock")
	return nil
}
