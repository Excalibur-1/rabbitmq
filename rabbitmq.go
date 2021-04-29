package rabbitmq

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Excalibur-1/configuration"
	"github.com/streadway/amqp"
)

// 基于RabbitMQ的ESB提供器，其初始化参数格式如下：
// {
//   "brokerURL" : "amqp://guest:guest@localhost:5672/",  // RabbitMQ server的连接地址
//   "username" : "admin",  // 登录RabbitMQ的账号
//   "password" : "admin",  // 登录RabbitMQ的密码
// }
const (
	app   = "base"
	group = "esb"
	tag   = ""
)

var (
	StateClosed    = uint8(0)
	StateOpened    = uint8(1)
	StateReopening = uint8(2)
)

type Client struct {
	url         string           // RabbitMQ连接的url
	mutex       sync.RWMutex     // 保护内部数据并发读写
	conn        *amqp.Connection // RabbitMQ TCP连接
	producerMap map[string]*Producer
	consumerMap map[string]*Consumer
	closeC      chan *amqp.Error // RabbitMQ 监听连接错误
	stopC       chan struct{}    // 监听用户手动关闭
	state       uint8            // MQ状态
}

func Engine(conf configuration.Configuration, namespace, systemId string) *Client {
	fmt.Println("Loading RabbitMQ Engine ver:1.0.0")
	var cfg map[string]string
	if err := conf.Clazz(namespace, app, group, tag, systemId, &cfg); err == nil {
		return New(cfg["brokerURL"], cfg["username"], cfg["password"])
	} else {
		panic("加载ESB实例出错")
	}
}

func New(brokerURL, username, password string) *Client {
	url := fmt.Sprintf("amqp://%s:%s@%s/", username, password, brokerURL)
	return &Client{
		url:         url,
		state:       StateClosed,
		producerMap: make(map[string]*Producer),
		consumerMap: make(map[string]*Consumer),
	}
}

func (c *Client) Open() (mq *Client, err error) {
	// 进行Open期间不允许做任何跟连接有关的事情
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.state == StateOpened {
		return c, errors.New("RabbitMQ had been opened")
	}

	if c.conn, err = amqp.Dial(c.url); err != nil {
		return c, fmt.Errorf("RabbitMQ Dial err: %v", err)
	}

	c.state = StateOpened
	c.stopC = make(chan struct{})
	c.closeC = make(chan *amqp.Error, 1)
	c.conn.NotifyClose(c.closeC)
	go c.keepalive()
	return c, nil
}

func (c *Client) Producer(name string) (*Producer, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.state != StateOpened {
		return nil, fmt.Errorf("MQ: Not initialized, now state is %b", c.State())
	}
	p, ok := c.producerMap[name]
	if !ok {
		p = newProducer(name, c)
		c.producerMap[name] = p
	}

	return p, nil
}

func (c *Client) CloseProducer(name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.state != StateOpened {
		return fmt.Errorf("MQ: Not initialized, now state is %b", c.State())
	}
	if p, ok := c.producerMap[name]; ok {
		p.Close()
		return nil
	}
	return errors.New("MQ: producer not exist")
}

func (c *Client) Consumer(name string) (*Consumer, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.state != StateOpened {
		return nil, fmt.Errorf("MQ: Not initialized, now state is %b", c.State())
	}
	consumer, ok := c.consumerMap[name]
	if !ok {
		consumer = newConsumer(name, c)
		c.consumerMap[name] = consumer
	}
	return consumer, nil
}

func (c *Client) CloseConsumer(name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.state != StateOpened {
		return fmt.Errorf("MQ: Not initialized, now state is %b", c.State())
	}
	if consumer, ok := c.consumerMap[name]; ok {
		consumer.Close()
		return nil
	}
	return errors.New("MQ: producer not exist")
}

func (c *Client) Close() {
	log.Printf("RabbitMQ client Close...\n")
	c.mutex.Lock()

	// Close producers
	for _, p := range c.producerMap {
		p.Close()
	}
	c.producerMap = make(map[string]*Producer)

	// Close consumers
	for _, co := range c.consumerMap {
		co.Close()
	}
	c.consumerMap = make(map[string]*Consumer)

	// Close mq connection
	select {
	case <-c.stopC:
		// had been closed
	default:
		close(c.stopC)
	}

	c.mutex.Unlock()

	// wait done
	for c.State() != StateClosed {
		time.Sleep(time.Second)
	}
}

func (c *Client) State() uint8 {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.state
}

func (c *Client) keepalive() {
	select {
	case <-c.stopC:
		// 正常关闭
		log.Printf("Shutdown RabbitMQ normally.\n")
		c.mutex.Lock()
		_ = c.conn.Close()
		c.state = StateClosed
		c.mutex.Unlock()

	case err := <-c.closeC:
		if err == nil {
			log.Printf("Disconnected with RabbitMQ, but Error detail is nil\n")
		} else {
			log.Printf("Disconnected with RabbitMQ, code:%d, reason:%s\n", err.Code, err.Reason)
		}

		// tcp连接中断, 重新连接
		c.mutex.Lock()
		c.state = StateReopening
		c.mutex.Unlock()

		maxRetry := 99999999
		for i := 0; i < maxRetry; i++ {
			time.Sleep(time.Second)
			if _, e := c.Open(); e != nil {
				log.Printf("Connection RabbitMQ recover failed for %d times, %v\n", i+1, e)
				continue
			}
			log.Printf("Connection RabbitMQ recover OK. Total try %d times\n", i+1)
			return
		}
		log.Printf("Try to reconnect to RabbitMQ failed over maxRetry(%d), so exit.\n", maxRetry)
	}
}

func (c *Client) channel() (*amqp.Channel, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.conn.Channel()
}
