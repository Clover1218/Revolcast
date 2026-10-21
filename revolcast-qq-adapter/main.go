package main

import (
	"revolcast-qq-adapter/internal/config"
	"revolcast-qq-adapter/internal/pkg/logger"

	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/driver"
	"go.uber.org/zap"
)

func main() {

	cfg, err := config.LoadConfig(".")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}
	log, err := logger.New(&cfg.Log)
	if err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer log.Sync()

	log.Info("Revolcast 启动中",
		zap.String("service", cfg.App.Name),
		zap.String("env", cfg.App.Env),
	)

	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()

	// client := transport.NewWSClient(ctx, "ws://127.0.0.1:8080/ws?token=123&bot_id=abc", log)
	// go client.Start()

	// time.Sleep(2 * time.Second)

	// log.Info("🚀 开始压力测试，准备发送 10 万条消息...")
	// startTime := time.Now()

	// // 3. 启动 100 个并发，每个发 1000 条
	// var wg sync.WaitGroup
	// concurrency := 100
	// messagesPerWorker := 1000

	// for i := 0; i < concurrency; i++ {
	// 	wg.Add(1)
	// 	go func(workerID int) {
	// 		defer wg.Done()
	// 		for j := 0; j < messagesPerWorker; j++ {
	// 			client.SendEvent(map[string]interface{}{
	// 				"user_id": workerID, // 模拟不同的用户
	// 				"msg":     fmt.Sprintf("压测消息 %d", j),
	// 			})
	// 		}
	// 	}(i)
	// }

	// wg.Wait()
	// duration := time.Since(startTime)
	// log.Info("✅ 压测发送完毕",
	// 	zap.Int("total", concurrency*messagesPerWorker),
	// 	zap.Duration("耗时", duration),
	// )

	// // 留点时间给 Worker 消费队列
	// time.Sleep(5 * time.Second)

	enableNapcat := true
	if enableNapcat { // 初始化ZeroBot
		zero.OnCommand("hello").Handle(func(ctx *zero.Ctx) {
			ctx.Send("world")
			log.Info("接受到Hello，回复World", zap.String("sender_name:", ctx.Event.Sender.NickName), zap.Int64("time", ctx.Event.Time))
		})
		zero.RunAndBlock(&zero.Config{
			NickName:      []string{"bot"},
			CommandPrefix: "",
			SuperUsers:    []int64{1457095790},
			Driver: []zero.Driver{
				driver.NewWebSocketClient("ws://127.0.0.1:6701", "J5xTRRESL-Fsdq1b"),
			},
		}, nil)

	}

}
