// s3.go wires the real AWS SDK signer into the S3 backend.
//
// It is in its own file so the S3 dependency is only pulled in when the
// backend is actually selected: a build that never uses S3 still compiles
// this file, but the config that selects it is in main.go, and an operator
// who never sets FIELDSVC_MEDIA_BACKEND=s3 never pays for the client.
package media

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// RealS3Signer is the production S3Signer, using the AWS SDK presign client.
type RealS3Signer struct {
	presigner *s3.PresignClient
}

// NewRealS3Signer builds the signer over an S3 client.
func NewRealS3Signer(client *s3.Client) *RealS3Signer {
	return &RealS3Signer{presigner: s3.NewPresignClient(client)}
}

// PresignPutObject returns a presigned PUT URL.
func (s *RealS3Signer) PresignPutObject(ctx context.Context, bucket, key, contentType string, expires time.Duration) (string, error) {
	out, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}
