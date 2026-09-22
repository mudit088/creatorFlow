// Package storage wraps object storage behind a small interface. Every method
// here either signs a URL the browser will use directly or asks S3 a question
// about an object. Deliberately absent: anything that reads or writes object
// bytes. Bytes never pass through this process.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mudit/creatorflow/backend/internal/config"
)

type Client struct {
	s3        *s3.Client
	presigner *s3.PresignClient
	bucket    string
}

// New builds an S3 client. Nothing below this line is AWS-specific: MinIO,
// Cloudflare R2 and Backblaze B2 all speak the same API, so switching providers
// is a change to S3_ENDPOINT and two keys, not a change to any code.
//
// UsePathStyle is what makes a local MinIO work. Real S3 addresses a bucket as
// bucket.s3.amazonaws.com (virtual-host style), which requires DNS for every
// bucket name. A local container has no such DNS, so the bucket has to live in
// the path instead: localhost:9000/creatorflow-dev/key.
func New(cfg *config.Config) *Client {
	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.Region = cfg.S3Region
			o.Credentials = credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")
			o.UsePathStyle = cfg.S3ForcePathStyle
			if cfg.S3Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			}
		},
	}

	client := s3.New(s3.Options{}, opts...)
	return &Client{
		s3:        client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.S3Bucket,
	}
}

// PresignUpload returns a URL the browser can PUT one specific object to.
//
// The signature covers Content-Type and Content-Length, not just the key. That
// is the whole security model of this endpoint: the client declared a 12 MB PDF,
// so the signed URL will only accept a 12 MB PDF. A client that tries to push
// 80 GB, or to swap the declared PDF for an HTML page that would then be served
// from our domain, produces a request whose headers no longer match what was
// signed, and S3 rejects it before a byte of body is read.
//
// Without those bound headers a presigned PUT is an unbounded write grant to
// whoever holds the link.
func (c *Client) PresignUpload(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	req, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign upload for %s: %w", key, err)
	}
	return req.URL, nil
}

// PresignDownload returns a short-lived GET URL. Phase 9 hands these to buyers
// after an entitlement check; the bucket itself stays private, so the URL is the
// only way in and it stops working on its own.
func (c *Client) PresignDownload(ctx context.Context, key, downloadAs string, ttl time.Duration) (string, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if downloadAs != "" {
		// Makes the browser save the creator's original filename rather than
		// displaying a uuid, and forces a download instead of rendering the file
		// inline, which is what stops an uploaded .html from executing as a page.
		in.ResponseContentDisposition = aws.String(contentDisposition(downloadAs))
	}

	req, err := c.presigner.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign download for %s: %w", key, err)
	}
	return req.URL, nil
}

// ObjectInfo is what the confirm step checks the upload against.
type ObjectInfo struct {
	Size int64
	ETag string
}

// StatObject asks S3 whether the object really landed, and how big it is.
//
// This is why confirming an upload is a separate call and not something the
// client simply asserts. The browser uploads directly, so the API never sees the
// transfer succeed or fail; taking the client's word for it would let anyone
// mark a product deliverable with nothing behind it, and the buyer would find
// that out after paying.
func (c *Client) StatObject(ctx context.Context, key string) (*ObjectInfo, bool, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errorsAs(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("head object %s: %w", key, err)
	}

	info := &ObjectInfo{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ETag != nil {
		info.ETag = *out.ETag
	}
	return info, true, nil
}

// DeleteObject removes an object. Used to clean up after an upload that was
// presigned and then abandoned, so the bucket does not slowly fill with files no
// product row points at.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}
