/*
 * Minimal object storage library (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package objectstorage

import (
	"errors"
	"io"
	"runtime"
	"sort"
)

// API - object storage API interface
type API interface {
	// Object Read/Write/Stat operations
	ObjectAPI

	// Bucket Read/Write/Stat operations
	BucketAPI
}

// ObjectAPI - object specific Read/Write/Stat interface
type ObjectAPI interface {
	GetObject(bucket, object string, offset, length uint64) (io.ReadCloser, *ObjectMetadata, error)
	CreateObject(bucket, object string, size uint64, data io.Reader) (string, error)
	StatObject(bucket, object string) (*ObjectMetadata, error)
	DeleteObject(bucket, object string) error
}

// BucketAPI - bucket specific Read/Write/Stat interface
type BucketAPI interface {
	CreateBucket(bucket, acl string) error
	SetBucketACL(bucket, acl string) error
	StatBucket(bucket string) error
	DeleteBucket(bucket string) error

	ListObjects(bucket, prefix string, recursive bool) <-chan ObjectOnChannel
	ListBuckets() <-chan BucketOnChannel
}

// ObjectOnChannel - object metadata over read channel
type ObjectOnChannel struct {
	Data *ObjectMetadata
	Err  error
}

// BucketOnChannel - bucket metadata over read channel
type BucketOnChannel struct {
	Data *BucketMetadata
	Err  error
}

type api struct {
	*lowLevelAPI
}

// Config - main configuration struct used by all to set endpoint, credentials, and other options for requests.
type Config struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string
	ContentType     string
	// not exported internal usage only
	userAgent string
}

// Global constants
const (
	LibraryName    = "objectstorage-go/"
	LibraryVersion = "0.1"
)

// New - instantiate a new minio api client
func New(config *Config) API {
	// Not configurable at the moment, but we will relook on this in future
	config.userAgent = LibraryName + " (" + LibraryVersion + "; " + runtime.GOOS + "; " + runtime.GOARCH + ")"
	return &api{&lowLevelAPI{config}}
}

/// Object operations

// GetObject retrieve object
//
// Additionally it also takes range arguments to download the specified range bytes of an object.
// For more information about the HTTP Range header, go to http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35.
func (a *api) GetObject(bucket, object string, offset, length uint64) (io.ReadCloser, *ObjectMetadata, error) {
	// get the the object
	// NOTE : returned md5sum could be the md5sum of the partial object itself
	// not the whole object depending on if offset range was requested or not
	body, objectMetadata, err := a.getObject(bucket, object, offset, length)
	if err != nil {
		return nil, nil, err
	}
	return body, objectMetadata, nil
}

// completedParts is a wrapper to make parts sortable by their part number
// multi part completion requires list of multi parts to be sorted
type completedParts []*CompletePart

func (a completedParts) Len() int           { return len(a) }
func (a completedParts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a completedParts) Less(i, j int) bool { return a[i].PartNumber < a[j].PartNumber }

// DefaultPartSize - default size per object after which PutObject becomes multipart
var DefaultPartSize uint64 = 1024 * 1024 * 5

// CreateObject create an object in a bucket
//
// You must have WRITE permissions on a bucket to create an object
//
// This version of CreateObject automatically does multipart for more than 5MB worth of data
// This default part size is not configurable currently but can be configurable in future
func (a *api) CreateObject(bucket, object string, size uint64, data io.Reader) (string, error) {
	switch {
	case size < DefaultPartSize:
		// Single Part use case, use PutObject directly
		for part := range Parts(data, DefaultPartSize) {
			if part.Err != nil {
				return "", part.Err
			}
			return "", a.putObject(bucket, object, part.Len, part.Data)
		}
	default:
		initiateMultipartUploadResult, err := a.initiateMultipartUpload(bucket, object)
		if err != nil {
			return "", err
		}
		uploadID := initiateMultipartUploadResult.UploadID
		completeMultipartUpload := new(CompleteMultipartUpload)
		for part := range Parts(data, DefaultPartSize) {
			if part.Err != nil {
				return "", part.Err
			}
			completePart, err := a.uploadPart(bucket, object, uploadID, part.Num, part.Len, part.Data)
			if err != nil {
				return "", a.abortMultipartUpload(bucket, object, uploadID)
			}
			completeMultipartUpload.Part = append(completeMultipartUpload.Part, completePart)
		}
		sort.Sort(completedParts(completeMultipartUpload.Part))
		completeMultipartUploadResult, err := a.completeMultipartUpload(bucket, object, uploadID, completeMultipartUpload)
		if err != nil {
			return "", a.abortMultipartUpload(bucket, object, uploadID)
		}
		return completeMultipartUploadResult.ETag, nil
	}
	return "", errors.New("Unexpected control flow")
}

// StatObject verify if object exists and you have permission to access it
func (a *api) StatObject(bucket, object string) (*ObjectMetadata, error) {
	return a.headObject(bucket, object)
}

// DeleteObject remove the object from a bucket
func (a *api) DeleteObject(bucket, object string) error {
	return a.deleteObject(bucket, object)
}

/// Bucket operations

// CreateBucket create a new bucket
func (a *api) CreateBucket(bucket, acl string) error {
	return a.putBucket(bucket, acl)
}

// SetBucketACL set the permissions on an existing bucket using access control lists (ACL)
//
// Currently supported are:
// ------------------
// private - owner gets full access
// public-read - owner gets full access, others get read access
// public-read-write - owner gets full access, others get full access too
// ------------------
func (a *api) SetBucketACL(bucket, acl string) error {
	return a.putBucketACL(bucket, acl)
}

// StatBucket verify if bucket exists and you have permission to access it
func (a *api) StatBucket(bucket string) error {
	return a.headBucket(bucket)
}

// DeleteBucket deletes the bucket named in the URI
// NOTE: -
//  All objects (including all object versions and delete markers)
//  in the bucket must be deleted before successfully attempting this request
func (a *api) DeleteBucket(bucket string) error {
	return a.deleteBucket(bucket)
}

// listObjectsInRoutine is an internal goroutine function called for listing objects
// This function feeds data into channel
func (a *api) listObjectsInRoutine(bucket, prefix string, recursive bool, ch chan ObjectOnChannel) {
	defer close(ch)
	switch {
	case recursive == true:
		listBucketResult, err := a.listObjects(bucket, 1000, "", prefix, "")
		if err != nil {
			ch <- ObjectOnChannel{
				Data: nil,
				Err:  err,
			}
			return
		}
		for _, object := range listBucketResult.Contents {
			ch <- ObjectOnChannel{
				Data: object,
				Err:  nil,
			}
		}
		for {
			if !listBucketResult.IsTruncated {
				break
			}
			listBucketResult, err = a.listObjects(bucket, 1000, listBucketResult.Marker, prefix, "")
			if err != nil {
				ch <- ObjectOnChannel{
					Data: nil,
					Err:  err,
				}
				return
			}
			for _, object := range listBucketResult.Contents {
				ch <- ObjectOnChannel{
					Data: object,
					Err:  nil,
				}
				listBucketResult.Marker = object.Key
			}
		}
	default:
		listBucketResult, err := a.listObjects(bucket, 1000, "", prefix, "/")
		if err != nil {
			ch <- ObjectOnChannel{
				Data: nil,
				Err:  err,
			}
			return
		}
		for _, object := range listBucketResult.Contents {
			ch <- ObjectOnChannel{
				Data: object,
				Err:  nil,
			}
		}
	}
}

// ListObjects - (List Objects) - List some objects or all recursively
//
// ListObjects is a channel based API implemented to facilitate ease of usage of S3 API ListObjects()
// by automatically recursively traversing all objects on a given bucket if specified.
//
// Your input paramters are just bucket, prefix and recursive
//
// If you enable recursive as 'true' this function will return back all the objects in a given bucket
//
//  eg:-
//         api := objectstorage.New(....)
//         for message := range api.ListObjects("mytestbucket", "starthere", true) {
//                 fmt.Println(message.Data)
//         }
//
func (a *api) ListObjects(bucket string, prefix string, recursive bool) <-chan ObjectOnChannel {
	ch := make(chan ObjectOnChannel)
	go a.listObjectsInRoutine(bucket, prefix, recursive, ch)
	return ch
}

// listBucketsInRoutine is an internal go routine function called for listing buckets
// This function feeds data into channel
func (a *api) listBucketsInRoutine(ch chan BucketOnChannel) {
	defer close(ch)
	listAllMyBucketListResults, err := a.listBuckets()
	if err != nil {
		ch <- BucketOnChannel{
			Data: nil,
			Err:  err,
		}
		return
	}
	for _, bucket := range listAllMyBucketListResults.Buckets.Bucket {
		ch <- BucketOnChannel{
			Data: bucket,
			Err:  nil,
		}
	}

}

// ListBuckets list of all buckets owned by the authenticated sender of the request
//
// NOTE:
//     This call requires explicit authentication, no anonymous
//     requests are allowed for listing buckets
//
//  eg:-
//         api := objectstorage.New(....)
//         for message := range api.ListBuckets() {
//                 fmt.Println(message.Data)
//         }
//
func (a *api) ListBuckets() <-chan BucketOnChannel {
	ch := make(chan BucketOnChannel)
	go a.listBucketsInRoutine(ch)
	return ch
}
