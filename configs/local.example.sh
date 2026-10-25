#!/usr/bin/env bash
# 先用 compose.yaml + compose.debug.yaml 启动依赖与 catalog/payment，停止 trade 容器后调试。
# Broker 公告 host.docker.internal，本机也必须能解析该名称；具体说明见 README。
# source configs/local.example.sh
# go run ./cmd/trade
export MYSQL_DSN='dingdong_trade:demo-trade@tcp(localhost:13306)/dingdong_trade'
export JWT_SECRET='dingdong-demo-signing-key-change-me-32bytes'
export PAYMENT_WINDOW='120s'
export HOSTNAME='trade-local'
# 多实例必须使用一致的限流配置；演示故障默认关闭。
export USER_RATE_BURST='3'
export USER_RATE_PER_SECOND='1'
export ACTIVITY_RATE_BURST='500'
export ACTIVITY_RATE_PER_SECOND='100'
export CACHE_ORIGIN_LIMIT='16'
export WORKER_CONCURRENCY='8'
export RETRY_LIMIT='20'
export DEMO_FAULTS_ENABLED='false'
export HTTP_ADDR=':18082'
export REDIS_ADDR='localhost:16379'
export CATALOG_RPC_TARGET='dns:///localhost:19000'
export PAYMENT_RPC_TARGET='dns:///localhost:19001'
export ROCKETMQ_NAMESERVER='localhost:19876'
# 本机探测使用映射端口；真实 MQ 通信使用 NameServer 返回的 Broker 公告地址。
export ROCKETMQ_BROKER_ADDR='localhost:10911'
