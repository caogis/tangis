// Package objectstore 封装 MinIO 对象存储访问：产物上传与按 key 读取。
//
// 配置全部来自环境变量（默认值与 deploy/.env.example 对齐）：
//   - MINIO_ENDPOINT    默认 127.0.0.1:19000
//   - MINIO_ACCESS_KEY  默认 tangis
//   - MINIO_SECRET_KEY  默认 tangis_dev_secret
//   - MINIO_BUCKET      默认 tangis
//   - MINIO_USE_SSL     默认 false
//
// MinIO 不可用时调用方负责降级：worker 跳过上传仅告警、服务发布接口
// 仅走本地文件，不阻塞主链路。
package objectstore

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Uploader 产物上传接口，worker 依赖它而非具体 MinIO 实现，便于单测 mock。
type Uploader interface {
	// UploadDir 将 localDir 下全部文件上传至 prefix 前缀之下
	// （保留相对目录结构）；任一文件失败即返回 error。
	UploadDir(ctx context.Context, localDir, prefix string) error
}

// Getter 按 key 读取对象，服务发布接口的 MinIO 回源依赖它。
type Getter interface {
	// Get 返回对象内容流与大小；对象不存在返回 error。
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// ObjectInfo 对象枚举条目（Lister 返回）。
type ObjectInfo struct {
	Key  string
	Size int64
}

// Lister 按前缀枚举对象，打包下载的 MinIO 产物清单依赖它。
type Lister interface {
	// List 返回前缀之下全部对象（不含目录占位）。
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// GetterLister 读+列组合能力（打包下载回源使用，*Client 实现）。
type GetterLister interface {
	Getter
	Lister
}

// Config MinIO 连接配置。
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Location  string
	UseSSL    bool
}

// ConfigFromEnv 从环境变量读取配置（默认值对齐 deploy/.env.example）。
func ConfigFromEnv() Config {
	return Config{
		Endpoint:  envOr("MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: envOr("MINIO_ACCESS_KEY", "tangis"),
		SecretKey: envOr("MINIO_SECRET_KEY", "tangis_dev_secret"),
		Bucket:    envOr("MINIO_BUCKET", "tangis"),
		Location:  envOr("MINIO_LOCATION", "us-east-1"),
		UseSSL:    envOr("MINIO_USE_SSL", "false") == "true",
	}
}

// Client MinIO 客户端，同时实现 Uploader 与 Getter。
type Client struct {
	cli *minio.Client
	cfg Config
}

// New 创建客户端并确保 bucket 存在（不存在则创建）。
// 连接/鉴权失败返回 error，由调用方决定降级策略；ctx 建议带超时。
func New(ctx context.Context, cfg Config) (*Client, error) {
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: new client %s: %w", cfg.Endpoint, err)
	}
	ok, err := cli.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: probe bucket %s: %w", cfg.Bucket, err)
	}
	if !ok {
		if err := cli.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Location}); err != nil {
			return nil, fmt.Errorf("objectstore: create bucket %s: %w", cfg.Bucket, err)
		}
	}
	return &Client{cli: cli, cfg: cfg}, nil
}

// Bucket 返回当前 bucket 名。
func (c *Client) Bucket() string { return c.cfg.Bucket }

// UploadDir 实现 Uploader：遍历目录，按相对路径上传到 prefix 之下。
func (c *Client) UploadDir(ctx context.Context, localDir, prefix string) error {
	return filepath.WalkDir(localDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		key := path.Join(prefix, filepath.ToSlash(rel))
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		_, err = c.cli.PutObject(ctx, c.cfg.Bucket, key, f, st.Size(),
			minio.PutObjectOptions{ContentType: ContentType(rel)})
		if err != nil {
			return fmt.Errorf("objectstore: put %s: %w", key, err)
		}
		return nil
	})
}

// Get 实现 Getter：按 key 流式读取对象；先 Stat 校验存在性，
// 对象不存在时返回 error（不返回惰性流）。
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := c.cli.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, 0, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	return obj, st.Size, nil
}

// List 实现 Lister：按前缀枚举对象（跳过目录占位与空 key）。
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
	var out []ObjectInfo
	for obj := range c.cli.ListObjects(ctx, c.cfg.Bucket, opts) {
		if obj.Err != nil {
			return nil, fmt.Errorf("objectstore: list %s: %w", prefix, obj.Err)
		}
		if obj.Key == "" || strings.HasSuffix(obj.Key, "/") {
			continue
		}
		out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size})
	}
	return out, nil
}

// ContentType 按扩展名返回对象 Content-Type：
// json → application/json；b3dm 等二进制瓦片 → application/octet-stream。
func ContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// DialTimeout 建连探测的默认超时。
const DialTimeout = 3 * time.Second
