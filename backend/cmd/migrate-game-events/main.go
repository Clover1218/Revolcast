// 一次性迁移工具：把 game_events 中旧格式的 received_at / occurred_at（ISO8601 字符串）
// 转换为 BSON Date，使游标分页（received_at + _id）具备正确的时间序。
//
// 背景：旧实现以 time.RFC3339Nano 写字符串，尾部 0 被裁剪导致小数位长度不定，
// 字符串字典序不等于时间序（`...34.5Z` > `...34.522048Z`，但时间更早）。
// 详见 docs/Revolcast_HTTP接口设计文档.md §13.7、docs/鹊桥与服务端WS连接格式详解.md 公共约定。
//
// 用法（在 backend 目录下执行，连接参数取自 config.yaml / REVOLCAST_ 环境变量）：
//
//	go run cmd/migrate-game-events/main.go            # 真正执行
//	go run cmd/migrate-game-events/main.go -dry-run   # 只统计，不写入
//
// 幂等：只处理仍为字符串的字段；已迁移为 Date 的文档会被跳过，可安全重复执行。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"revolcast-backend/internal/config"
	"revolcast-backend/internal/repository"
)

const (
	collectionName = "game_events"
	batchSize      = 200
	runTimeout     = 5 * time.Minute
)

// legacyDoc 用 any 承载时间字段，以便同时兼容旧（string）与新（BSON Date）两种存储形态。
type legacyDoc struct {
	ID         primitive.ObjectID `bson:"_id"`
	OccurredAt any                `bson:"occurred_at"`
	ReceivedAt any                `bson:"received_at"`
}

func main() {
	dryRun := flag.Bool("dry-run", false, "只统计待迁移文档，不写入数据库")
	flag.Parse()

	db, err := repository.NewMongoDB(&mustConfig().MongoDB)
	if err != nil {
		fatal("MongoDB 连接失败", err)
	}
	coll := db.Collection(collectionName)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	// 只取仍含字符串时间的文档：已迁移的文档不会被再次处理（幂等）。
	filter := bson.M{"$or": bson.A{
		bson.M{"received_at": bson.M{"$type": "string"}},
		bson.M{"occurred_at": bson.M{"$type": "string"}},
	}}

	total, converted, skipped, failed := 0, 0, 0, 0
	var models []mongo.WriteModel

	cur, err := coll.Find(ctx, filter, options.Find().SetBatchSize(batchSize))
	if err != nil {
		fatal("查询待迁移文档失败", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	flush := func() {
		if len(models) == 0 {
			return
		}
		if *dryRun {
			models = models[:0]
			return
		}
		if _, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
			// 整批失败时回退到逐条处理，避免一条坏文档阻塞其余数据迁移。
			fatal("批量写入失败", err)
		}
		models = models[:0]
	}

	for cur.Next(ctx) {
		var doc legacyDoc
		if err := cur.Decode(&doc); err != nil {
			failed++
			fmt.Printf("[skip] _id=%s 解码失败: %v\n", doc.ID.Hex(), err)
			continue
		}
		total++

		set := bson.M{}
		for field, raw := range map[string]any{"occurred_at": doc.OccurredAt, "received_at": doc.ReceivedAt} {
			s, ok := raw.(string)
			if !ok {
				continue // 已是 Date 或缺失，跳过
			}
			t, perr := parseLegacyTime(s)
			if perr != nil {
				failed++
				fmt.Printf("[skip] _id=%s %s=%q 解析失败: %v\n", doc.ID.Hex(), field, s, perr)
				continue
			}
			set[field] = t
		}
		if len(set) == 0 {
			skipped++
			continue
		}

		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": doc.ID}).
			SetUpdate(bson.M{"$set": set}))
		converted++
		if len(models) >= batchSize {
			flush()
		}
	}
	if err := cur.Err(); err != nil {
		fatal("遍历文档失败", err)
	}
	flush()

	mode := "已迁移"
	if *dryRun {
		mode = "待迁移(dry-run)"
	}
	fmt.Printf("集合 %s：扫描 %d 条，%s %d 条，跳过 %d 条，失败 %d 条\n",
		collectionName, total, mode, converted, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// parseLegacyTime 兼容历史写入格式：RFC3339Nano（实际写入值）与 RFC3339。
func parseLegacyTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// mustConfig 加载配置（与 cmd/server 一致，从当前目录读取 config.yaml）。
func mustConfig() *config.Config {
	cfg, err := config.LoadConfig(".")
	if err != nil {
		fatal("加载配置失败", err)
	}
	return cfg
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}
