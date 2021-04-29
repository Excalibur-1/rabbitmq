package rabbitmq

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/streadway/amqp"
)

// Producer 基于RabbitMQ的生产者封装。
type Producer struct {
	name          string                 // Producer的名字, "" is OK
	client        *Client                // MQ实例
	chPool        *sync.Pool             // channel pool, 重复使用
	publishMutex  sync.RWMutex           // publish数据的锁
	mutex         sync.RWMutex           // 保护数据安全地并发读写
	ch            *amqp.Channel          // MQ的会话channel
	exchangeBinds []*ExchangeBinds       // MQ的exchange与其绑定的queues
	enableConfirm bool                   // 生产者confirm开关
	confirmC      chan amqp.Confirmation // 监听publish confirm
	confirm       *confirmHelper         // confirm结果检测
	closeC        chan *amqp.Error       // 监听会话channel关闭
	stopC         chan struct{}          // Producer关闭控制
	state         uint8                  // Producer 状态
}

func newProducer(name string, client *Client) *Producer {
	return &Producer{
		name:   name,
		client: client,
		chPool: &sync.Pool{
			New: func() interface{} { return make(chan bool, 1) },
		},
		state: StateClosed,
	}
}

func (p *Producer) Name() string {
	return p.name
}

// Confirm 是否开启生产者confirm功能, 默认为false, 该选项在Open()前设置.
// 说明: 目前仅实现串行化的confirm, 每次的等待confirm额外需要约50ms,建议上层并发调用Publish
func (p *Producer) Confirm(enable bool) *Producer {
	p.mutex.Lock()
	p.enableConfirm = enable
	p.mutex.Unlock()
	return p
}

func (p *Producer) SetExchangeBinds(eb []*ExchangeBinds) *Producer {
	p.mutex.Lock()
	if p.state != StateOpened {
		p.exchangeBinds = eb
	}
	p.mutex.Unlock()
	return p
}

func (p *Producer) IsOpen() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.state == StateOpened
}

func (p *Producer) Open() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// 条件检测
	if p.client == nil {
		return errors.New("rabbit mq Bad producer")
	}
	if len(p.exchangeBinds) <= 0 {
		return errors.New("rabbit mq No exchangeBinds found. You should SetExchangeBinds before open")
	}
	if p.state == StateOpened {
		return errors.New("rabbit mq Producer had been opened")
	}

	// 创建并初始化channel
	ch, err := p.client.channel()
	if err != nil {
		return fmt.Errorf("rabbit mq Create channel failed, %v", err)
	}
	if err = applyExchangeBinds(ch, p.exchangeBinds); err != nil {
		_ = ch.Close()
		return err
	}

	p.ch = ch
	p.state = StateOpened

	// 初始化发送Confirm
	if p.enableConfirm {
		p.confirmC = make(chan amqp.Confirmation, 1) // channel关闭时自动关闭
		_ = p.ch.Confirm(false)
		p.ch.NotifyPublish(p.confirmC)
		if p.confirm == nil {
			p.confirm = newConfirmHelper()
		} else {
			p.confirm.Reset()
		}

		go p.listenConfirm()
	}

	// 初始化Keepalive
	if true {
		p.stopC = make(chan struct{})
		p.closeC = make(chan *amqp.Error, 1) // channel关闭时自动关闭
		p.ch.NotifyClose(p.closeC)

		go p.keepalive()
	}

	return nil
}

func (p *Producer) Close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	select {
	case <-p.stopC:
		// had been closed
	default:
		close(p.stopC)
	}
}

func (p *Producer) ForDirect(exchange, route, queue, data string) error {
	return p.ForQueueArgs(exchange, route, queue, amqp.ExchangeDirect, []byte(data), nil, nil)
}

func (p *Producer) ForTopic(exchange, route, queue, data string) error {
	return p.ForQueueArgs(exchange, route, queue, amqp.ExchangeTopic, []byte(data), nil, nil)
}

func (p *Producer) ForFanout(exchange, route, queue, data string) error {
	return p.ForQueueArgs(exchange, route, queue, amqp.ExchangeFanout, []byte(data), nil, nil)
}

func (p *Producer) ForDelay(exchange, route, queue, data string) error {
	return p.ForQueueArgs(exchange, route, queue, "x-delayed-message", []byte(data), amqp.Table{"x-delayed-type": "direct"}, nil)
}

func (p *Producer) ForQueueArgs(exchange, route, queue, kind string, data []byte, exchangeArgs, queueArgs amqp.Table) error {
	p.SetExchangeBinds([]*ExchangeBinds{
		{
			Exch:     DefaultExchange(exchange, kind, exchangeArgs),
			Bindings: []*Binding{{RouteKey: route, Queues: []*Queue{DefaultQueue(queue, queueArgs)}}},
		},
	})
	if !p.IsOpen() {
		if err := p.Open(); err != nil {
			return fmt.Errorf("[ERROR] Open failed, %v", err)
		}
	}
	return p.Publish(exchange, route, NewPublishMsg(data, exchangeArgs))
}

// Publish 在同步Publish Confirm模式下, 每次Publish将额外有约50ms的等待时间.如果采用这种模式,建议上层并发publish
func (p *Producer) Publish(exchange, route string, msg *PublishMsg) error {
	if msg == nil {
		return errors.New("rabbit mq Nil publish msg")
	}

	if st := p.State(); st != StateOpened {
		return fmt.Errorf("rabbit mq Producer unopened, now state is %d", p.state)
	}

	pub := amqp.Publishing{
		Headers:         msg.headers,
		ContentType:     msg.ContentType,
		ContentEncoding: msg.ContentEncoding,
		DeliveryMode:    msg.DeliveryMode,
		Priority:        msg.Priority,
		Timestamp:       msg.Timestamp,
		Body:            msg.Body,
	}

	// 非confirm模式
	if p.enableConfirm == false {
		return p.ch.Publish(exchange, route, false, false, pub)
	}

	// confirm模式
	// 这里加锁保证消息发送顺序与接收ack的channel的编号一致
	p.publishMutex.Lock()
	if err := p.ch.Publish(exchange, route, false, false, pub); err != nil {
		p.publishMutex.Unlock()
		return fmt.Errorf("rabbit mq Producer publish failed, %v", err)
	}
	ch := p.chPool.Get().(chan bool)
	p.confirm.Listen(ch)
	p.publishMutex.Unlock()

	ack, ok := <-ch
	p.chPool.Put(ch)
	if !ack || !ok {
		return fmt.Errorf("rabbit mq Producer publish failed, confirm ack is false. ack:%t, ok:%t", ack, ok)
	}
	return nil
}

func (p *Producer) State() uint8 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.state
}

func (p *Producer) keepalive() {
	select {
	case <-p.stopC:
		// 正常关闭
		fmt.Printf("Rabbit mq Producer(%s) shutdown normally.\n", p.name)
		p.mutex.Lock()
		_ = p.ch.Close()
		p.state = StateClosed
		p.mutex.Unlock()

	case err := <-p.closeC:
		if err == nil {
			log.Printf("RabbitMq Producer(%s)'s channel was closed, but Error detail is nil\n", p.name)
		} else {
			log.Printf("RabbitMq Producer(%s)'s channel was closed, code:%d, reason:%s\n", p.name, err.Code, err.Reason)
		}

		// channel被异常关闭了
		p.mutex.Lock()
		p.state = StateReopening
		p.mutex.Unlock()

		maxRetry := 99999999
		for i := 0; i < maxRetry; i++ {
			time.Sleep(time.Second)
			if p.client.State() != StateOpened {
				fmt.Printf("Rabbit mq Producer(%s) try to recover channel for %d times, but mq's state != StateOpened\n", p.name, i+1)
				continue
			}
			if e := p.Open(); e != nil {
				fmt.Printf("Rabbit mq Producer(%s) recover channel failed for %d times, Err:%v\n", p.name, i+1, e)
				continue
			}
			fmt.Printf("Rabbit mq Producer(%s) recover channel OK. Total try %d times\n", p.name, i+1)
			return
		}
		log.Printf("Rabbit mq Producer(%s) try to recover channel over maxRetry(%d), so exit\n", p.name, maxRetry)
	}
}

func (p *Producer) listenConfirm() {
	for c := range p.confirmC {
		// TODO: 可以做个并发控制
		go p.confirm.Callback(c.DeliveryTag, c.Ack)
	}
}

func applyExchangeBinds(ch *amqp.Channel, exchangeBinds []*ExchangeBinds) (err error) {
	if ch == nil {
		return errors.New("rabbit mq Nil producer channel")
	}
	if len(exchangeBinds) <= 0 {
		return errors.New("rabbit mq Empty exchangeBinds")
	}

	for _, eb := range exchangeBinds {
		if eb.Exch == nil {
			return errors.New("rabbit mq Nil exchange found")
		}
		ex := eb.Exch
		// topic 或者 fan-out 时， bindings 可以为空
		if (ex.Kind == amqp.ExchangeFanout || ex.Kind == amqp.ExchangeTopic) && len(eb.Bindings) == 0 {
			continue
		} else if len(eb.Bindings) <= 0 {
			return fmt.Errorf("rabbit mq No bindings queue found for exchange(%s)", eb.Exch.Name)
		}
		// declare exchange
		if err = ch.ExchangeDeclare(ex.Name, ex.Kind, ex.Durable, ex.AutoDelete, ex.Internal, ex.NoWait, ex.Args); err != nil {
			return fmt.Errorf("rabbit mq Declare exchange(%s) failed, %v", ex.Name, err)
		}
		// declare and bind queues
		for _, b := range eb.Bindings {
			if b == nil {
				return fmt.Errorf("rabbit mq Nil binding found, exchange:%s", ex.Name)
			}
			if len(b.Queues) <= 0 {
				return fmt.Errorf("rabbit mq No queues found for exchange(%s)", ex.Name)
			}
			for _, q := range b.Queues {
				if q == nil {
					return fmt.Errorf("rabbit mq Nil queue found, exchange:%s", ex.Name)
				}
				if _, err = ch.QueueDeclare(q.Name, q.Durable, q.AutoDelete, q.Exclusive, q.NoWait, q.Args); err != nil {
					return fmt.Errorf("rabbit mq Declare queue(%s) failed, %v", q.Name, err)
				}
				if err = ch.QueueBind(q.Name, b.RouteKey, ex.Name, b.NoWait, b.Args); err != nil {
					return fmt.Errorf("rabbit mq Bind exchange(%s) <--> queue(%s) failed, %v", ex.Name, q.Name, err)
				}
			}
		}
	}
	return
}

type confirmHelper struct {
	mutex     sync.RWMutex
	listeners map[uint64]chan<- bool
	count     uint64
}

func newConfirmHelper() *confirmHelper {
	h := confirmHelper{}
	return h.Reset()
}

func (h *confirmHelper) Reset() *confirmHelper {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// 解除所有等待listener返回ACK的阻塞的地方
	for _, ch := range h.listeners {
		close(ch)
	}

	// Reset
	h.count = uint64(0)
	h.listeners = make(map[uint64]chan<- bool)
	return h
}

func (h *confirmHelper) Listen(ch chan<- bool) {
	h.mutex.Lock()
	h.count++
	h.listeners[h.count] = ch
	h.mutex.Unlock()
}

func (h *confirmHelper) Callback(idx uint64, ack bool) {
	h.mutex.Lock()
	ch := h.listeners[idx]
	delete(h.listeners, idx)
	h.mutex.Unlock()
	ch <- ack
}
