package main

import (
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mattn/go-zglob"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Plugin defines the S3 plugin parameters.
type Plugin struct {
	Endpoint              string
	Key                   string
	Secret                string
	AssumeRole            string
	AssumeRoleSessionName string
	Bucket                string
	UserRoleArn           string

	// if not "", enable server-side encryption
	// valid values are:
	//     AES256
	//     aws:kms
	Encryption string

	// us-east-1
	// us-west-1
	// us-west-2
	// eu-west-1
	// ap-southeast-1
	// ap-southeast-2
	// ap-northeast-1
	// sa-east-1
	Region string

	// Indicates the files ACL, which should be one
	// of the following:
	//     private
	//     public-read
	//     public-read-write
	//     authenticated-read
	//     bucket-owner-read
	//     bucket-owner-full-control
	Access string

	// Sets the content type on each uploaded object based on a extension map
	ContentType map[string]string

	// Sets the content encoding on each uploaded object based on a extension map
	ContentEncoding map[string]string

	// Sets the Cache-Control header on each uploaded object based on a extension map
	CacheControl map[string]string

	// Sets the storage class, affects the storage backend costs
	StorageClass string

	// Copies the files from the specified directory.
	// Regexp matching will apply to match multiple
	// files
	//
	// Examples:
	//    /path/to/file
	//    /path/to/*.txt
	//    /path/to/*/*.txt
	//    /path/to/**
	Source string
	Target string

	// Strip the prefix from the target path
	StripPrefix string

	// Exclude files matching this pattern.
	Exclude []string

	// Use path style instead of domain style.
	//
	// Should be true for minio and false for AWS.
	PathStyle bool
	// if true, plugin is set to download mode, which means `target` from the bucket will be downloaded
	Download bool
	// Dry run without uploading/
	DryRun bool
}

// Exec runs the plugin
func (p *Plugin) Exec() error {
	// normalize the target URL
	if strings.HasPrefix(p.Target, "/") {
		p.Target = p.Target[1:]
	}

	// create the client
	conf := &aws.Config{
		Region:           aws.String(p.Region),
		Endpoint:         &p.Endpoint,
		DisableSSL:       aws.Bool(strings.HasPrefix(p.Endpoint, "http://")),
		S3ForcePathStyle: aws.Bool(p.PathStyle),
	}

	if p.Key != "" && p.Secret != "" {
		conf.Credentials = credentials.NewStaticCredentials(p.Key, p.Secret, "")
	} else if p.AssumeRole != "" {
		conf.Credentials = assumeRole(p.AssumeRole, p.AssumeRoleSessionName)
	} else {
		log.Warn("AWS Key and/or Secret not provided (falling back to ec2 instance profile)")
	}

	var client *s3.S3
	sess, err := session.NewSession(conf)
	if err != nil {
		log.WithError(err).Errorln("could not instantiate session")
		return err
	}

	// If user role ARN is set then assume role here
	if len(p.UserRoleArn) > 0 {
		confRoleArn := aws.Config{
			Region:      aws.String(p.Region),
			Credentials: stscreds.NewCredentials(sess, p.UserRoleArn),
		}

		client = s3.New(sess, &confRoleArn)
	} else {
		client = s3.New(sess)
	}

	if p.Download {
		targetDir := strings.TrimPrefix(filepath.ToSlash(p.Target), "/")
		log.WithFields(log.Fields{
			"bucket": p.Bucket,
			"dir":    targetDir,
		}).Info("Listing S3 directory")

		list, err := client.ListObjectsV2(&s3.ListObjectsV2Input{
			Bucket: &p.Bucket,
			Prefix: &targetDir,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"error":  err,
				"bucket": p.Bucket,
				"dir":    targetDir,
			}).Error("Cannot list S3 directory")
			return err
		}

		g := errgroup.Group{}

		for _, item := range list.Contents {
			log.WithFields(log.Fields{
				"bucket": p.Bucket,
				"key":    *item.Key,
			}).Info("Getting S3 object")

			item := item
			g.Go(func() error {
				obj, err := client.GetObject(&s3.GetObjectInput{
					Bucket: &p.Bucket,
					Key:    item.Key,
				})
				if err != nil {
					log.WithFields(log.Fields{
						"error":  err,
						"bucket": p.Bucket,
						"key":    *item.Key,
					}).Error("Cannot get S3 object")
					return err
				}

				source := resolveSource(targetDir, *item.Key, p.StripPrefix)

				f, err := os.Create(source)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
						"file":  source,
					}).Error("Problem opening file for writing")
					return err
				}
				defer f.Close()

				_, err = io.Copy(f, obj.Body)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
						"file":  source,
					}).Error("Failed to write file")
					return err
				}

				return nil
			})
		}

		return g.Wait()
	}

	// find the bucket
	log.WithFields(log.Fields{
		"region":   p.Region,
		"endpoint": p.Endpoint,
		"bucket":   p.Bucket,
	}).Info("Attempting to upload")

	matches, err := matches(p.Source, p.Exclude)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("Could not match files")
		return err
	}

	for _, match := range matches {

		stat, err := os.Stat(match)
		if err != nil {
			continue // should never happen
		}

		// skip directories
		if stat.IsDir() {
			continue
		}

		target := resolveKey(p.Target, match, p.StripPrefix)

		contentType := matchExtension(match, p.ContentType)
		contentEncoding := matchExtension(match, p.ContentEncoding)
		cacheControl := matchExtension(match, p.CacheControl)

		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(match))

			if contentType == "" {
				contentType = "application/octet-stream"
			}
		}

		// log file for debug purposes.
		log.WithFields(log.Fields{
			"name":   match,
			"bucket": p.Bucket,
			"target": target,
		}).Info("Uploading file")

		// when executing a dry-run we exit because we don't actually want to
		// upload the file to S3.
		if p.DryRun {
			continue
		}

		f, err := os.Open(match)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
				"file":  match,
			}).Error("Problem opening file")
			return err
		}
		defer f.Close()

		putObjectInput := &s3.PutObjectInput{
			Body:   f,
			Bucket: &(p.Bucket),
			Key:    &target,
			ACL:    &(p.Access),
		}

		if contentType != "" {
			putObjectInput.ContentType = aws.String(contentType)
		}

		if contentEncoding != "" {
			putObjectInput.ContentEncoding = aws.String(contentEncoding)
		}

		if cacheControl != "" {
			putObjectInput.CacheControl = aws.String(cacheControl)
		}

		if p.Encryption != "" {
			putObjectInput.ServerSideEncryption = aws.String(p.Encryption)
		}

		if p.StorageClass != "" {
			putObjectInput.StorageClass = &(p.StorageClass)
		}

		_, err = client.PutObject(putObjectInput)

		if err != nil {
			log.WithFields(log.Fields{
				"name":   match,
				"bucket": p.Bucket,
				"target": target,
				"error":  err,
			}).Error("Could not upload file")

			return err
		}
		f.Close()
	}

	return nil
}

// matches is a helper function that returns a list of all files matching the
// included Glob pattern, while excluding all files that matche the exclusion
// Glob pattners.
func matches(include string, exclude []string) ([]string, error) {
	matches, err := zglob.Glob(include)
	if err != nil {
		return nil, err
	}
	if len(exclude) == 0 {
		return matches, nil
	}

	// find all files that are excluded and load into a map. we can verify
	// each file in the list is not a member of the exclusion list.
	excludem := map[string]bool{}
	for _, pattern := range exclude {
		excludes, err := zglob.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range excludes {
			excludem[match] = true
		}
	}

	var included []string
	for _, include := range matches {
		_, ok := excludem[include]
		if ok {
			continue
		}
		included = append(included, include)
	}
	return included, nil
}

func matchExtension(match string, stringMap map[string]string) string {
	for pattern := range stringMap {
		matched, err := regexp.MatchString(pattern, match)

		if err != nil {
			panic(err)
		}

		if matched {
			return stringMap[pattern]
		}
	}

	return ""
}

func assumeRole(roleArn, roleSessionName string) *credentials.Credentials {
	client := sts.New(session.New())
	duration := time.Hour * 1
	stsProvider := &stscreds.AssumeRoleProvider{
		Client:          client,
		Duration:        duration,
		RoleARN:         roleArn,
		RoleSessionName: roleSessionName,
	}

	return credentials.NewCredentials(stsProvider)
}

// resolveKey is a helper function that returns s3 object key where file present at srcPath is uploaded to.
// srcPath is assumed to be in forward slash format
func resolveKey(target, srcPath, stripPrefix string) string {
	key := filepath.Join(target, strings.TrimPrefix(srcPath, filepath.ToSlash(stripPrefix)))
	key = filepath.ToSlash(key)
	if !strings.HasPrefix(key, "/") {
		key = "/" + key
	}
	return key
}

func resolveSource(targetDir, target, stripPrefix string) string {
	path := strings.TrimPrefix(strings.TrimPrefix(target, targetDir), "/")
	return stripPrefix + path
}
