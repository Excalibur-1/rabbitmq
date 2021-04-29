package main

import (
	"fmt"
	"log"

	"github.com/Excalibur-1/configuration"
	"github.com/Excalibur-1/rabbitmq"
	"github.com/streadway/amqp"
)

func main() {
	conf := configuration.MockEngine(map[string]string{
		"/fc/base/esb/9999": "{\"username\":\"guest\",\"password\":\"guest\",\"brokerURL\":\"localhost:5672\"}",
	})
	client, err := rabbitmq.Engine(conf, "myconf", "9999").Open()
	if err != nil {
		fmt.Println(err)
		return
	}
	ex := "sys_esb_9999"
	if p, err := client.Producer(ex); err == nil {
		defer client.Close() // 关闭 client 会清理所有相关 producer & consumer
		kind := "x-delayed-message"
		queue := "delay_9999"
		route := "delay_9999"
		data := []byte("延迟消息")
		p.SetExchangeBinds([]*rabbitmq.ExchangeBinds{
			{
				Exch:     rabbitmq.DefaultExchange(ex, kind, amqp.Table{"x-delayed-type": "direct"}),
				Bindings: []*rabbitmq.Binding{{RouteKey: route, Queues: []*rabbitmq.Queue{rabbitmq.DefaultQueue(queue, nil)}}},
			},
		})
		if !p.IsOpen() {
			if err := p.Open(); err != nil {
				fmt.Printf("[ERROR] Open failed, %v\n", err)
				return
			}
		}
		if err := p.Publish(ex, route, rabbitmq.NewPublishMsg(data, amqp.Table{"x-delay": 10000})); err != nil {
			fmt.Printf("[ERROR] Publish failed, %v\n", err)
		}
	} else {
		log.Printf("init client has error:%+v\n", err)
	}
}
