package repository

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"revolcast-backend/internal/config"
)

// NewMongoDB 根据配置连接 MongoDB 并探活，返回选中的 Database 句柄。
func NewMongoDB(cfg *config.MongoDBConfig) (*mongo.Database, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uri := fmt.Sprintf(
		"mongodb://%s:%s@%s:%d",
		cfg.User, cfg.Password, cfg.Host, cfg.Port,
	)

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("连接 MongoDB 失败: %w", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("MongoDB 探活失败: %w", err)
	}

	return client.Database(cfg.DBName), nil
}
