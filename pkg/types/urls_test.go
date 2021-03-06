// Copyright 2018 MOBIKE, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"strings"
	"testing"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) {
	TestingT(t)
}

var _ = Suite(&testTypesSuite{})

type testTypesSuite struct{}

func (s *testTypesSuite) TestURLs(c *C) {
	urlStrs := []string{
		"http://127.0.0.1:80",
		"http://localhost:8080",
		"http://www.facebook.com:9090",
	}

	urls, err := NewURLs(urlStrs)
	c.Assert(err, IsNil)
	c.Assert(urls.String(), Equals, strings.Join(urlStrs, ","))
}

func (s *testTypesSuite) TestInvalidURLs(c *C) {
	invalidCases := [][]string{
		{"http://192.168.0.2"},
		{"127.0.0.1:1008"},
		{"http://192.168.0.1:80/api"},
		{},
	}

	for _, invalidCase := range invalidCases {
		_, err := NewURLs(invalidCase)
		c.Assert(err, NotNil)
	}
}
