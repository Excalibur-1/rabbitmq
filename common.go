package rabbitmq

import (
	"time"

	"github.com/streadway/amqp"
)

// Binding routeKey ==> queues
type Binding struct {
	RouteKey string
	Queues   []*Queue
	NoWait   bool       // default is false
	Args     amqp.Table // default is nil
}

// Exchange 基于amqp的Exchange配置
type Exchange struct {
	Name       string
	Kind       string
	Durable    bool
	AutoDelete bool
	Internal   bool
	NoWait     bool
	Args       amqp.Table // default is nil
}

// ExchangeBinds exchange ==> routeKey ==> queues
type ExchangeBinds struct {
	Exch     *Exchange
	Bindings []*Binding
}

func DefaultExchange(name string, kind string, exchangeArgs amqp.Table) *Exchange {
	return &Exchange{
		Name:       name,
		Kind:       kind,
		Durable:    true,
		AutoDelete: false,
		Internal:   false,
		NoWait:     false,
		Args:       exchangeArgs,
	}
}

// Queue 基于amqp的Queue配置
type Queue struct {
	Name       string
	Durable    bool
	AutoDelete bool
	Exclusive  bool
	NoWait     bool
	Args       amqp.Table
}

func DefaultQueue(name string, queueArgs amqp.Table) *Queue {
	return &Queue{
		Name:       name,
		Durable:    true,
		AutoDelete: false,
		Exclusive:  false,
		NoWait:     false,
		Args:       queueArgs,
	}
}

// PublishMsg 生产者生产的数据格式
type PublishMsg struct {
	ContentType     string // MIME content type
	ContentEncoding string // MIME content type
	DeliveryMode    uint8  // Transient or Persistent
	Priority        uint8  // 0 to 9
	Timestamp       time.Time
	Body            []byte
	headers         amqp.Table
}

func NewPublishMsg(body []byte, headers amqp.Table) *PublishMsg {
	return &PublishMsg{
		ContentType:     "application/json",
		ContentEncoding: "",
		DeliveryMode:    amqp.Persistent,
		Priority:        uint8(5),
		Timestamp:       time.Now(),
		Body:            body,
		headers:         headers,
	}
}

// ConsumeOption 消费者消费选项
type ConsumeOption struct {
	AutoAck   bool
	Exclusive bool
	NoLocal   bool
	NoWait    bool
	Args      amqp.Table
}

func DefaultConsumeOption() *ConsumeOption {
	return &ConsumeOption{NoWait: true}
}
