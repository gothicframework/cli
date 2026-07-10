package gothicwasm

import . "github.com/gothicframework/core/wasm"

type CounterState struct {
	Count int
}

var _ = CreateTopic(CounterState{}, TopicConfig{
	Name:             "counter",
	Compression:      BROTLI,
	SubscriberFnName: "GetCounterTopic",
})
