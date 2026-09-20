package config

import "os"

// Config 集中承载运行期配置，全部可通过环境变量覆盖，便于容器化部署。
type Config struct {
	// HTTP 服务
	ServerPort string

	// MySQL
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string

	// Redis
	RedisHost string
	RedisPort string
	RedisPass string

	// AI 服务（Python 侧）
	AIServiceURL string
}

func Load() *Config {
	return &Config{
		ServerPort: getEnv("SERVER_PORT", "8080"),

		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "3306"),
		DBUser:     getEnv("DB_USER", "root"),
		DBPassword: getEnv("DB_PASSWORD", "123456"),
		DBName:     getEnv("DB_NAME", "community"),

		RedisHost: getEnv("REDIS_HOST", "localhost"),
		RedisPort: getEnv("REDIS_PORT", "6379"),
		RedisPass: getEnv("REDIS_PASSWORD", ""),

		AIServiceURL: getEnv("AI_SERVICE_URL", "http://localhost:8000"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
