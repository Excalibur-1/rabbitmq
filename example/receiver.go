package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Excalibur-1/configuration"
	"github.com/Excalibur-1/rabbitmq"
	"github.com/streadway/amqp"
)

func main() {
	conf := configuration.MockEngine(map[string]string{
		"/fc/base/esb/client": "{\"enabled\":true}",
		"/fc/base/esb/9999":   "{\"username\":\"guest\",\"password\":\"guest\",\"brokerURL\":\"localhost:5672\"}",
	})
	client, _ := rabbitmq.Engine(conf, "myconf", "9999").Open()
	ex := "sys_esb_9999"
	queue := "delay_9999"
	kind := "x-delayed-message"
	route := "delay_9999"
	if consumer, err := client.Consumer("sys_esb"); err == nil {
		// 关闭 client 会清理所有相关 producer & consumer
		defer client.Close()
		log.Println("receiver listening...")
		consumer.SetExchangeBinds([]*rabbitmq.ExchangeBinds{
			{
				Exch: rabbitmq.DefaultExchange(ex, kind, amqp.Table{"x-delayed-type": "direct"}),
				Bindings: []*rabbitmq.Binding{
					{
						RouteKey: route,
						Queues: []*rabbitmq.Queue{
							// 一个消费者对应一个队列
							rabbitmq.DefaultQueue(queue, nil),
						},
					},
				},
			},
		})

		if err = consumer.Open(); err == nil {
			msgC := make(chan rabbitmq.Delivery, 1)
			consumer.SetMsgCallback(msgC)
			consumer.SetQos(10)
			go func() {
				for msg := range msgC {
					log.Printf("Tag(%d) Body: %s\n", msg.DeliveryTag, string(msg.Body))
					_ = msg.Ack(true)
				}
				log.Println("go routine exit.")
			}()
			defer close(msgC)
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
			for {
				s := <-c
				log.Printf("receiver receive a signal: %s\n", s.String())
				switch s {
				case syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT:
					log.Println("receiver exit")
					return
				default:
					return
				}
			}
		} else {
			log.Printf("init Consumer has error:%+v\n", err)
		}
	} else {
		log.Printf("init client has error:%+v\n", err)
	}
}
