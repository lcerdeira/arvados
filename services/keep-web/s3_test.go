// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"git.arvados.org/arvados.git/sdk/go/arvados"
	"git.arvados.org/arvados.git/sdk/go/arvadosclient"
	"git.arvados.org/arvados.git/sdk/go/arvadostest"
	"git.arvados.org/arvados.git/sdk/go/keepclient"
	"github.com/AdRoll/goamz/aws"
	"github.com/AdRoll/goamz/s3"
	check "gopkg.in/check.v1"
)

type s3stage struct {
	arv        *arvados.Client
	ac         *arvadosclient.ArvadosClient
	kc         *keepclient.KeepClient
	proj       arvados.Group
	projbucket *s3.Bucket
	coll       arvados.Collection
	collbucket *s3.Bucket
}

func (s *IntegrationSuite) s3setup(c *check.C) s3stage {
	var proj arvados.Group
	var coll arvados.Collection
	arv := arvados.NewClientFromEnv()
	arv.AuthToken = arvadostest.ActiveToken
	err := arv.RequestAndDecode(&proj, "POST", "arvados/v1/groups", nil, map[string]interface{}{
		"group": map[string]interface{}{
			"group_class": "project",
			"name":        "keep-web s3 test",
		},
		"ensure_unique_name": true,
	})
	c.Assert(err, check.IsNil)
	err = arv.RequestAndDecode(&coll, "POST", "arvados/v1/collections", nil, map[string]interface{}{"collection": map[string]interface{}{
		"owner_uuid":    proj.UUID,
		"name":          "keep-web s3 test collection",
		"manifest_text": ". d41d8cd98f00b204e9800998ecf8427e+0 0:0:emptyfile\n./emptydir d41d8cd98f00b204e9800998ecf8427e+0 0:0:.\n",
	}})
	c.Assert(err, check.IsNil)
	ac, err := arvadosclient.New(arv)
	c.Assert(err, check.IsNil)
	kc, err := keepclient.MakeKeepClient(ac)
	c.Assert(err, check.IsNil)
	fs, err := coll.FileSystem(arv, kc)
	c.Assert(err, check.IsNil)
	f, err := fs.OpenFile("sailboat.txt", os.O_CREATE|os.O_WRONLY, 0644)
	c.Assert(err, check.IsNil)
	_, err = f.Write([]byte("⛵\n"))
	c.Assert(err, check.IsNil)
	err = f.Close()
	c.Assert(err, check.IsNil)
	err = fs.Sync()
	c.Assert(err, check.IsNil)
	err = arv.RequestAndDecode(&coll, "GET", "arvados/v1/collections/"+coll.UUID, nil, nil)
	c.Assert(err, check.IsNil)

	auth := aws.NewAuth(arvadostest.ActiveTokenUUID, arvadostest.ActiveToken, "", time.Now().Add(time.Hour))
	region := aws.Region{
		Name:       s.testServer.Addr,
		S3Endpoint: "http://" + s.testServer.Addr,
	}
	client := s3.New(*auth, region)
	client.Signature = aws.V4Signature
	return s3stage{
		arv:  arv,
		ac:   ac,
		kc:   kc,
		proj: proj,
		projbucket: &s3.Bucket{
			S3:   client,
			Name: proj.UUID,
		},
		coll: coll,
		collbucket: &s3.Bucket{
			S3:   client,
			Name: coll.UUID,
		},
	}
}

func (stage s3stage) teardown(c *check.C) {
	if stage.coll.UUID != "" {
		err := stage.arv.RequestAndDecode(&stage.coll, "DELETE", "arvados/v1/collections/"+stage.coll.UUID, nil, nil)
		c.Check(err, check.IsNil)
	}
	if stage.proj.UUID != "" {
		err := stage.arv.RequestAndDecode(&stage.proj, "DELETE", "arvados/v1/groups/"+stage.proj.UUID, nil, nil)
		c.Check(err, check.IsNil)
	}
}

func (s *IntegrationSuite) TestS3Signatures(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	bucket := stage.collbucket
	for _, trial := range []struct {
		success   bool
		signature int
		accesskey string
		secretkey string
	}{
		{true, aws.V2Signature, arvadostest.ActiveToken, "none"},
		{false, aws.V2Signature, "none", "none"},
		{false, aws.V2Signature, "none", arvadostest.ActiveToken},

		{true, aws.V4Signature, arvadostest.ActiveTokenUUID, arvadostest.ActiveToken},
		{true, aws.V4Signature, arvadostest.ActiveToken, arvadostest.ActiveToken},
		{false, aws.V4Signature, arvadostest.ActiveToken, ""},
		{false, aws.V4Signature, arvadostest.ActiveToken, "none"},
		{false, aws.V4Signature, "none", arvadostest.ActiveToken},
		{false, aws.V4Signature, "none", "none"},
	} {
		c.Logf("%#v", trial)
		bucket.S3.Auth = *(aws.NewAuth(trial.accesskey, trial.secretkey, "", time.Now().Add(time.Hour)))
		bucket.S3.Signature = trial.signature
		_, err := bucket.GetReader("emptyfile")
		if trial.success {
			c.Check(err, check.IsNil)
		} else {
			c.Check(err, check.NotNil)
		}
	}
}

func (s *IntegrationSuite) TestS3HeadBucket(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	for _, bucket := range []*s3.Bucket{stage.collbucket, stage.projbucket} {
		c.Logf("bucket %s", bucket.Name)
		exists, err := bucket.Exists("")
		c.Check(err, check.IsNil)
		c.Check(exists, check.Equals, true)
	}
}

func (s *IntegrationSuite) TestS3CollectionGetObject(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3GetObject(c, stage.collbucket, "")
}
func (s *IntegrationSuite) TestS3ProjectGetObject(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3GetObject(c, stage.projbucket, stage.coll.Name+"/")
}
func (s *IntegrationSuite) testS3GetObject(c *check.C, bucket *s3.Bucket, prefix string) {
	rdr, err := bucket.GetReader(prefix + "emptyfile")
	c.Assert(err, check.IsNil)
	buf, err := ioutil.ReadAll(rdr)
	c.Check(err, check.IsNil)
	c.Check(len(buf), check.Equals, 0)
	err = rdr.Close()
	c.Check(err, check.IsNil)

	// GetObject
	rdr, err = bucket.GetReader(prefix + "missingfile")
	c.Check(err, check.ErrorMatches, `404 Not Found`)

	// HeadObject
	exists, err := bucket.Exists(prefix + "missingfile")
	c.Check(err, check.IsNil)
	c.Check(exists, check.Equals, false)

	// GetObject
	rdr, err = bucket.GetReader(prefix + "sailboat.txt")
	c.Assert(err, check.IsNil)
	buf, err = ioutil.ReadAll(rdr)
	c.Check(err, check.IsNil)
	c.Check(buf, check.DeepEquals, []byte("⛵\n"))
	err = rdr.Close()
	c.Check(err, check.IsNil)

	// HeadObject
	resp, err := bucket.Head(prefix+"sailboat.txt", nil)
	c.Check(err, check.IsNil)
	c.Check(resp.StatusCode, check.Equals, http.StatusOK)
	c.Check(resp.ContentLength, check.Equals, int64(4))
}

func (s *IntegrationSuite) TestS3CollectionPutObjectSuccess(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3PutObjectSuccess(c, stage.collbucket, "")
}
func (s *IntegrationSuite) TestS3ProjectPutObjectSuccess(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3PutObjectSuccess(c, stage.projbucket, stage.coll.Name+"/")
}
func (s *IntegrationSuite) testS3PutObjectSuccess(c *check.C, bucket *s3.Bucket, prefix string) {
	for _, trial := range []struct {
		path        string
		size        int
		contentType string
	}{
		{
			path:        "newfile",
			size:        128000000,
			contentType: "application/octet-stream",
		}, {
			path:        "newdir/newfile",
			size:        1 << 26,
			contentType: "application/octet-stream",
		}, {
			path:        "newdir1/newdir2/newfile",
			size:        0,
			contentType: "application/octet-stream",
		}, {
			path:        "newdir1/newdir2/newdir3/",
			size:        0,
			contentType: "application/x-directory",
		},
	} {
		c.Logf("=== %v", trial)

		objname := prefix + trial.path

		_, err := bucket.GetReader(objname)
		c.Assert(err, check.ErrorMatches, `404 Not Found`)

		buf := make([]byte, trial.size)
		rand.Read(buf)

		err = bucket.PutReader(objname, bytes.NewReader(buf), int64(len(buf)), trial.contentType, s3.Private, s3.Options{})
		c.Check(err, check.IsNil)

		rdr, err := bucket.GetReader(objname)
		if strings.HasSuffix(trial.path, "/") && !s.testServer.Config.cluster.Collections.S3FolderObjects {
			c.Check(err, check.NotNil)
			continue
		} else if !c.Check(err, check.IsNil) {
			continue
		}
		buf2, err := ioutil.ReadAll(rdr)
		c.Check(err, check.IsNil)
		c.Check(buf2, check.HasLen, len(buf))
		c.Check(bytes.Equal(buf, buf2), check.Equals, true)
	}
}

func (s *IntegrationSuite) TestS3ProjectPutObjectNotSupported(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	bucket := stage.projbucket

	for _, trial := range []struct {
		path        string
		size        int
		contentType string
	}{
		{
			path:        "newfile",
			size:        1234,
			contentType: "application/octet-stream",
		}, {
			path:        "newdir/newfile",
			size:        1234,
			contentType: "application/octet-stream",
		}, {
			path:        "newdir2/",
			size:        0,
			contentType: "application/x-directory",
		},
	} {
		c.Logf("=== %v", trial)

		_, err := bucket.GetReader(trial.path)
		c.Assert(err, check.ErrorMatches, `404 Not Found`)

		buf := make([]byte, trial.size)
		rand.Read(buf)

		err = bucket.PutReader(trial.path, bytes.NewReader(buf), int64(len(buf)), trial.contentType, s3.Private, s3.Options{})
		c.Check(err, check.ErrorMatches, `400 Bad Request`)

		_, err = bucket.GetReader(trial.path)
		c.Assert(err, check.ErrorMatches, `404 Not Found`)
	}
}

func (s *IntegrationSuite) TestS3CollectionDeleteObject(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3DeleteObject(c, stage.collbucket, "")
}
func (s *IntegrationSuite) TestS3ProjectDeleteObject(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3DeleteObject(c, stage.projbucket, stage.coll.Name+"/")
}
func (s *IntegrationSuite) testS3DeleteObject(c *check.C, bucket *s3.Bucket, prefix string) {
	s.testServer.Config.cluster.Collections.S3FolderObjects = true
	for _, trial := range []struct {
		path string
	}{
		{"/"},
		{"nonexistentfile"},
		{"emptyfile"},
		{"sailboat.txt"},
		{"sailboat.txt/"},
		{"emptydir"},
		{"emptydir/"},
	} {
		objname := prefix + trial.path
		comment := check.Commentf("objname %q", objname)

		err := bucket.Del(objname)
		if trial.path == "/" {
			c.Check(err, check.NotNil)
			continue
		}
		c.Check(err, check.IsNil, comment)
		_, err = bucket.GetReader(objname)
		c.Check(err, check.NotNil, comment)
	}
}

func (s *IntegrationSuite) TestS3CollectionPutObjectFailure(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3PutObjectFailure(c, stage.collbucket, "")
}
func (s *IntegrationSuite) TestS3ProjectPutObjectFailure(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	s.testS3PutObjectFailure(c, stage.projbucket, stage.coll.Name+"/")
}
func (s *IntegrationSuite) testS3PutObjectFailure(c *check.C, bucket *s3.Bucket, prefix string) {
	s.testServer.Config.cluster.Collections.S3FolderObjects = false

	// Can't use V4 signature for these tests, because
	// double-slash is incorrectly cleaned by the aws.V4Signature,
	// resulting in a "bad signature" error. (Cleaning the path is
	// appropriate for other services, but not in S3 where object
	// names "foo//bar" and "foo/bar" are semantically different.)
	bucket.S3.Auth = *(aws.NewAuth(arvadostest.ActiveToken, "none", "", time.Now().Add(time.Hour)))
	bucket.S3.Signature = aws.V2Signature

	var wg sync.WaitGroup
	for _, trial := range []struct {
		path string
	}{
		{
			path: "emptyfile/newname", // emptyfile exists, see s3setup()
		}, {
			path: "emptyfile/", // emptyfile exists, see s3setup()
		}, {
			path: "emptydir", // dir already exists, see s3setup()
		}, {
			path: "emptydir/",
		}, {
			path: "emptydir//",
		}, {
			path: "newdir/",
		}, {
			path: "newdir//",
		}, {
			path: "/",
		}, {
			path: "//",
		}, {
			path: "foo//bar",
		}, {
			path: "",
		},
	} {
		trial := trial
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Logf("=== %v", trial)

			objname := prefix + trial.path

			buf := make([]byte, 1234)
			rand.Read(buf)

			err := bucket.PutReader(objname, bytes.NewReader(buf), int64(len(buf)), "application/octet-stream", s3.Private, s3.Options{})
			if !c.Check(err, check.ErrorMatches, `400 Bad.*`, check.Commentf("PUT %q should fail", objname)) {
				return
			}

			if objname != "" && objname != "/" {
				_, err = bucket.GetReader(objname)
				c.Check(err, check.ErrorMatches, `404 Not Found`, check.Commentf("GET %q should return 404", objname))
			}
		}()
	}
	wg.Wait()
}

func (stage *s3stage) writeBigDirs(c *check.C, dirs int, filesPerDir int) {
	fs, err := stage.coll.FileSystem(stage.arv, stage.kc)
	c.Assert(err, check.IsNil)
	for d := 0; d < dirs; d++ {
		dir := fmt.Sprintf("dir%d", d)
		c.Assert(fs.Mkdir(dir, 0755), check.IsNil)
		for i := 0; i < filesPerDir; i++ {
			f, err := fs.OpenFile(fmt.Sprintf("%s/file%d.txt", dir, i), os.O_CREATE|os.O_WRONLY, 0644)
			c.Assert(err, check.IsNil)
			c.Assert(f.Close(), check.IsNil)
		}
	}
	c.Assert(fs.Sync(), check.IsNil)
}

func (s *IntegrationSuite) TestS3GetBucketVersioning(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)
	for _, bucket := range []*s3.Bucket{stage.collbucket, stage.projbucket} {
		req, err := http.NewRequest("GET", bucket.URL("/"), nil)
		c.Check(err, check.IsNil)
		req.Header.Set("Authorization", "AWS "+arvadostest.ActiveTokenV2+":none")
		req.URL.RawQuery = "versioning"
		resp, err := http.DefaultClient.Do(req)
		c.Assert(err, check.IsNil)
		c.Check(resp.Header.Get("Content-Type"), check.Equals, "application/xml")
		buf, err := ioutil.ReadAll(resp.Body)
		c.Assert(err, check.IsNil)
		c.Check(string(buf), check.Equals, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<VersioningConfiguration xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"/>\n")
	}
}

// If there are no CommonPrefixes entries, the CommonPrefixes XML tag
// should not appear at all.
func (s *IntegrationSuite) TestS3ListNoCommonPrefixes(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	req, err := http.NewRequest("GET", stage.collbucket.URL("/"), nil)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", "AWS "+arvadostest.ActiveTokenV2+":none")
	req.URL.RawQuery = "prefix=asdfasdfasdf&delimiter=/"
	resp, err := http.DefaultClient.Do(req)
	c.Assert(err, check.IsNil)
	buf, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, check.IsNil)
	c.Check(string(buf), check.Not(check.Matches), `(?ms).*CommonPrefixes.*`)
}

// If there is no delimiter in the request, or the results are not
// truncated, the NextMarker XML tag should not appear in the response
// body.
func (s *IntegrationSuite) TestS3ListNoNextMarker(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	for _, query := range []string{"prefix=e&delimiter=/", ""} {
		req, err := http.NewRequest("GET", stage.collbucket.URL("/"), nil)
		c.Assert(err, check.IsNil)
		req.Header.Set("Authorization", "AWS "+arvadostest.ActiveTokenV2+":none")
		req.URL.RawQuery = query
		resp, err := http.DefaultClient.Do(req)
		c.Assert(err, check.IsNil)
		buf, err := ioutil.ReadAll(resp.Body)
		c.Assert(err, check.IsNil)
		c.Check(string(buf), check.Not(check.Matches), `(?ms).*NextMarker.*`)
	}
}

// List response should include KeyCount field.
func (s *IntegrationSuite) TestS3ListKeyCount(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	req, err := http.NewRequest("GET", stage.collbucket.URL("/"), nil)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", "AWS "+arvadostest.ActiveTokenV2+":none")
	req.URL.RawQuery = "prefix=&delimiter=/"
	resp, err := http.DefaultClient.Do(req)
	c.Assert(err, check.IsNil)
	buf, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, check.IsNil)
	c.Check(string(buf), check.Matches, `(?ms).*<KeyCount>2</KeyCount>.*`)
}

func (s *IntegrationSuite) TestS3CollectionList(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	var markers int
	for markers, s.testServer.Config.cluster.Collections.S3FolderObjects = range []bool{false, true} {
		dirs := 2
		filesPerDir := 1001
		stage.writeBigDirs(c, dirs, filesPerDir)
		// Total # objects is:
		//                 2 file entries from s3setup (emptyfile and sailboat.txt)
		//                +1 fake "directory" marker from s3setup (emptydir) (if enabled)
		//             +dirs fake "directory" marker from writeBigDirs (dir0/, dir1/) (if enabled)
		// +filesPerDir*dirs file entries from writeBigDirs (dir0/file0.txt, etc.)
		s.testS3List(c, stage.collbucket, "", 4000, markers+2+(filesPerDir+markers)*dirs)
		s.testS3List(c, stage.collbucket, "", 131, markers+2+(filesPerDir+markers)*dirs)
		s.testS3List(c, stage.collbucket, "dir0/", 71, filesPerDir+markers)
	}
}
func (s *IntegrationSuite) testS3List(c *check.C, bucket *s3.Bucket, prefix string, pageSize, expectFiles int) {
	c.Logf("testS3List: prefix=%q pageSize=%d S3FolderObjects=%v", prefix, pageSize, s.testServer.Config.cluster.Collections.S3FolderObjects)
	expectPageSize := pageSize
	if expectPageSize > 1000 {
		expectPageSize = 1000
	}
	gotKeys := map[string]s3.Key{}
	nextMarker := ""
	pages := 0
	for {
		resp, err := bucket.List(prefix, "", nextMarker, pageSize)
		if !c.Check(err, check.IsNil) {
			break
		}
		c.Check(len(resp.Contents) <= expectPageSize, check.Equals, true)
		if pages++; !c.Check(pages <= (expectFiles/expectPageSize)+1, check.Equals, true) {
			break
		}
		for _, key := range resp.Contents {
			gotKeys[key.Key] = key
			if strings.Contains(key.Key, "sailboat.txt") {
				c.Check(key.Size, check.Equals, int64(4))
			}
		}
		if !resp.IsTruncated {
			c.Check(resp.NextMarker, check.Equals, "")
			break
		}
		if !c.Check(resp.NextMarker, check.Not(check.Equals), "") {
			break
		}
		nextMarker = resp.NextMarker
	}
	c.Check(len(gotKeys), check.Equals, expectFiles)
}

func (s *IntegrationSuite) TestS3CollectionListRollup(c *check.C) {
	for _, s.testServer.Config.cluster.Collections.S3FolderObjects = range []bool{false, true} {
		s.testS3CollectionListRollup(c)
	}
}

func (s *IntegrationSuite) testS3CollectionListRollup(c *check.C) {
	stage := s.s3setup(c)
	defer stage.teardown(c)

	dirs := 2
	filesPerDir := 500
	stage.writeBigDirs(c, dirs, filesPerDir)
	err := stage.collbucket.PutReader("dingbats", &bytes.Buffer{}, 0, "application/octet-stream", s3.Private, s3.Options{})
	c.Assert(err, check.IsNil)
	var allfiles []string
	for marker := ""; ; {
		resp, err := stage.collbucket.List("", "", marker, 20000)
		c.Check(err, check.IsNil)
		for _, key := range resp.Contents {
			if len(allfiles) == 0 || allfiles[len(allfiles)-1] != key.Key {
				allfiles = append(allfiles, key.Key)
			}
		}
		marker = resp.NextMarker
		if marker == "" {
			break
		}
	}
	markers := 0
	if s.testServer.Config.cluster.Collections.S3FolderObjects {
		markers = 1
	}
	c.Check(allfiles, check.HasLen, dirs*(filesPerDir+markers)+3+markers)

	gotDirMarker := map[string]bool{}
	for _, name := range allfiles {
		isDirMarker := strings.HasSuffix(name, "/")
		if markers == 0 {
			c.Check(isDirMarker, check.Equals, false, check.Commentf("name %q", name))
		} else if isDirMarker {
			gotDirMarker[name] = true
		} else if i := strings.LastIndex(name, "/"); i >= 0 {
			c.Check(gotDirMarker[name[:i+1]], check.Equals, true, check.Commentf("name %q", name))
			gotDirMarker[name[:i+1]] = true // skip redundant complaints about this dir marker
		}
	}

	for _, trial := range []struct {
		prefix    string
		delimiter string
		marker    string
	}{
		{"", "", ""},
		{"di", "/", ""},
		{"di", "r", ""},
		{"di", "n", ""},
		{"dir0", "/", ""},
		{"dir0/", "/", ""},
		{"dir0/f", "/", ""},
		{"dir0", "", ""},
		{"dir0/", "", ""},
		{"dir0/f", "", ""},
		{"dir0", "/", "dir0/file14.txt"},       // no commonprefixes
		{"", "", "dir0/file14.txt"},            // middle page, skip walking dir1
		{"", "", "dir1/file14.txt"},            // middle page, skip walking dir0
		{"", "", "dir1/file498.txt"},           // last page of results
		{"dir1/file", "", "dir1/file498.txt"},  // last page of results, with prefix
		{"dir1/file", "/", "dir1/file498.txt"}, // last page of results, with prefix + delimiter
		{"dir1", "Z", "dir1/file498.txt"},      // delimiter "Z" never appears
		{"dir2", "/", ""},                      // prefix "dir2" does not exist
		{"", "/", ""},
	} {
		c.Logf("\n\n=== trial %+v markers=%d", trial, markers)

		maxKeys := 20
		resp, err := stage.collbucket.List(trial.prefix, trial.delimiter, trial.marker, maxKeys)
		c.Check(err, check.IsNil)
		if resp.IsTruncated && trial.delimiter == "" {
			// goamz List method fills in the missing
			// NextMarker field if resp.IsTruncated, so
			// now we can't really tell whether it was
			// sent by the server or by goamz. In cases
			// where it should be empty but isn't, assume
			// it's goamz's fault.
			resp.NextMarker = ""
		}

		var expectKeys []string
		var expectPrefixes []string
		var expectNextMarker string
		var expectTruncated bool
		for _, key := range allfiles {
			full := len(expectKeys)+len(expectPrefixes) >= maxKeys
			if !strings.HasPrefix(key, trial.prefix) || key < trial.marker {
				continue
			} else if idx := strings.Index(key[len(trial.prefix):], trial.delimiter); trial.delimiter != "" && idx >= 0 {
				prefix := key[:len(trial.prefix)+idx+1]
				if len(expectPrefixes) > 0 && expectPrefixes[len(expectPrefixes)-1] == prefix {
					// same prefix as previous key
				} else if full {
					expectNextMarker = key
					expectTruncated = true
				} else {
					expectPrefixes = append(expectPrefixes, prefix)
				}
			} else if full {
				if trial.delimiter != "" {
					expectNextMarker = key
				}
				expectTruncated = true
				break
			} else {
				expectKeys = append(expectKeys, key)
			}
		}

		var gotKeys []string
		for _, key := range resp.Contents {
			gotKeys = append(gotKeys, key.Key)
		}
		var gotPrefixes []string
		for _, prefix := range resp.CommonPrefixes {
			gotPrefixes = append(gotPrefixes, prefix)
		}
		commentf := check.Commentf("trial %+v markers=%d", trial, markers)
		c.Check(gotKeys, check.DeepEquals, expectKeys, commentf)
		c.Check(gotPrefixes, check.DeepEquals, expectPrefixes, commentf)
		c.Check(resp.NextMarker, check.Equals, expectNextMarker, commentf)
		c.Check(resp.IsTruncated, check.Equals, expectTruncated, commentf)
		c.Logf("=== trial %+v keys %q prefixes %q nextMarker %q", trial, gotKeys, gotPrefixes, resp.NextMarker)
	}
}

// TestS3cmd checks compatibility with the s3cmd command line tool, if
// it's installed. As of Debian buster, s3cmd is only in backports, so
// `arvados-server install` don't install it, and this test skips if
// it's not installed.
func (s *IntegrationSuite) TestS3cmd(c *check.C) {
	if _, err := exec.LookPath("s3cmd"); err != nil {
		c.Skip("s3cmd not found")
		return
	}

	stage := s.s3setup(c)
	defer stage.teardown(c)

	cmd := exec.Command("s3cmd", "--no-ssl", "--host="+s.testServer.Addr, "--host-bucket="+s.testServer.Addr, "--access_key="+arvadostest.ActiveTokenUUID, "--secret_key="+arvadostest.ActiveToken, "ls", "s3://"+arvadostest.FooCollection)
	buf, err := cmd.CombinedOutput()
	c.Check(err, check.IsNil)
	c.Check(string(buf), check.Matches, `.* 3 +s3://`+arvadostest.FooCollection+`/foo\n`)
}
