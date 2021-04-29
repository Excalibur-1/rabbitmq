run_fan_out_sender: ## 运行示例发送方
	@go run sender.go

run_fan_out_receiver: ## 运行示例接收方
	@go run receiver.go

.DEFAULT_GOAL :=
help: ## show help info
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

