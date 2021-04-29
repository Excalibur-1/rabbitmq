package rabbitmq

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/streadway/amqp"
)

type Delivery struct {
	amqp.Delivery
}

// Consumer 基于RabbitMQ消息中间件的客户端实现。
type Consumer struct {
	name          string           // Consumer的名字, "" is OK
	client        *Client          // MQ实例
	mutex         sync.RWMutex     // 保护数据并发安全
	ch            *amqp.Channel    // MQ的会话channel
	exchangeBinds []*ExchangeBinds // MQ的exchange与其绑定的queues
	prefetch      int              // Oos prefetch
	callback      chan<- Delivery  // 上层用于接收消费出来的消息的管道
	closeC        chan *amqp.Error // 监听会话channel关闭
	stopC         chan struct{}    // Consumer关闭控制
	state         uint8            // Consumer状态
}

func newConsumer(name string, client *Client) *Consumer {
	return &Consumer{
		name:   name,
		client: client,
		stopC:  make(chan struct{}),
	}
}

func (c *Consumer) Name() string {
	return c.name
}

// CloseChan 该接口仅用于测试使用, 勿手动调用
func (c *Consumer) CloseChan() {
	c.mutex.Lock()
	_ = c.ch.Close()
	c.mutex.Unlock()
}

func (c *Consumer) SetExchangeBinds(eb []*ExchangeBinds) *Consumer {
	c.mutex.Lock()
	if c.state != StateOpened {
		c.exchangeBinds = eb
	}
	c.mutex.Unlock()
	return c
}

func (c *Consumer) SetMsgCallback(cb chan<- Delivery) *Consumer {
	c.mutex.Lock()
	c.callback = cb
	c.mutex.Unlock()
	return c
}

// SetQos 设置channel粒度的Qos, prefetch取值范围[0,∞), 默认为0
// 如果想要RoundRobin地进行消费，设置prefetch为1即可
// 注意:在调用Open前设置
func (c *Consumer) SetQos(prefetch int) *Consumer {
	c.mutex.Lock()
	c.prefetch = prefetch
	c.mutex.Unlock()
	return c
}

func (c *Consumer) Open() error {
	// Open期间不允许对channel做任何操作
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 参数校验
	if c.client == nil {
		return errors.New("rabbit mq bad consumer")
	}
	if len(c.exchangeBinds) <= 0 {
		return errors.New("rabbit mq no exchangeBinds found, you should SetExchangeBinds before open")
	}

	// 状态检测
	if c.state == StateOpened {
		return errors.New("rabbit mq  Consumer had been opened")
	}

	// 初始化channel
	ch, err := c.client.channel()
	if err != nil {
		return fmt.Errorf("rabbit mq Create channel failed, %v", err)
	}

	err = func(ch *amqp.Channel) error {
		var e error
		if e = applyExchangeBinds(ch, c.exchangeBinds); e != nil {
			return e
		}
		if e = ch.Qos(c.prefetch, 0, false); e != nil {
			return e
		}
		return nil
	}(ch)
	if err != nil {
		return fmt.Errorf("rabbit mq %v", err)
	}

	c.ch = ch
	c.state = StateOpened
	c.stopC = make(chan struct{})
	c.closeC = make(chan *amqp.Error, 1)
	c.ch.NotifyClose(c.closeC)

	// 开始循环消费
	opt := DefaultConsumeOption()
	notify := make(chan error, 1)
	c.consume(opt, notify)
	for e := range notify {
		if e != nil {
			log.Printf("[ERROR] %v\n", e)
			continue
		}
		break
	}
	close(notify)

	// 健康检测
	go c.keepalive()

	return nil
}

func (c *Consumer) Close() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	select {
	case <-c.stopC:
		// had been closed
	default:
		close(c.stopC)
	}
}

// notifyErr 向上层抛出错误, 如果error为空表示执行完成.由上层负责关闭channel
func (c *Consumer) consume(opt *ConsumeOption, notifyErr chan<- error) {
	for idx, eb := range c.exchangeBinds {
		if eb == nil {
			notifyErr <- fmt.Errorf("rabbit mq ExchangeBinds[%d] is nil, consumer(%s)", idx, c.name)
			continue
		}
		for i, b := range eb.Bindings {
			if b == nil {
				notifyErr <- fmt.Errorf("rabbit mq Binding[%d] is nil, ExchangeBinds[%d], consumer(%s)", i, idx, c.name)
				continue
			}
			for qi, q := range b.Queues {
				if q == nil {
					notifyErr <- fmt.Errorf("rabbit mq Queue[%d] is nil, ExchangeBinds[%d], Biding[%d], consumer(%s)", qi, idx, i, c.name)
					continue
				}
				delivery, err := c.ch.Consume(q.Name, "", opt.AutoAck, opt.Exclusive, opt.NoLocal, opt.NoWait, opt.Args)
				if err != nil {
					notifyErr <- fmt.Errorf("rabbit mq Consumer(%s) consume queue(%s) failed, %v", c.name, q.Name, err)
					continue
				}
				go c.deliver(delivery)
			}
		}
	}
	notifyErr <- nil
}

// FIXME 收到自己发出的消息
func (c *Consumer) deliver(delivery <-chan amqp.Delivery) {
	for d := range delivery {
		if c.callback != nil {
			c.callback <- Delivery{d}
		}
	}
}

func (c *Consumer) State() uint8 {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.state
}

func (c *Consumer) keepalive() {
	select {
	case <-c.stopC:
		// 正常关闭
		log.Printf("RabbitMQ Consumer(%s) shutdown normally\n", c.Name())
		c.mutex.Lock()
		_ = c.ch.Close()
		c.ch = nil
		c.state = StateClosed
		c.mutex.Unlock()

	case err := <-c.closeC:
		if err == nil {
			log.Printf("RabbitMQ Consumer(%s)'s channel was closed, but Error detail is nil\n", c.name)
		} else {
			log.Printf("RabbitMQ Consumer(%s)'s channel was closed, code:%d, reason:%s\n", c.name, err.Code, err.Reason)
		}

		// channel被异常关闭了
		c.mutex.Lock()
		c.state = StateReopening
		c.mutex.Unlock()

		maxRetry := 99999999
		for i := 0; i < maxRetry; i++ {
			time.Sleep(time.Second)
			if c.client.State() != StateOpened {
				fmt.Printf("Rabbit mq Consumer(%s) try to recover channel for %d times, but mq's state != StateOpened\n", c.name, i+1)
				continue
			}
			if e := c.Open(); e != nil {
				fmt.Printf("RabbitMQ Consumer(%s) recover channel failed for %d times, Err:%v\n", c.name, i+1, e)
				continue
			}
			fmt.Printf("RabbitMQ Consumer(%s) recover channel OK. Total try %d times\n", c.name, i+1)
			return
		}
		log.Printf("RabbitMQ Consumer(%s) try to recover channel over maxRetry(%d), so exit\n", c.name, maxRetry)
	}
}
