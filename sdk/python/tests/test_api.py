#!/usr/bin/env python

import arvados
import collections
import httplib2
import json
import mimetypes
import os
import run_test_server
import string
import unittest
from apiclient import errors as apiclient_errors
from apiclient import http as apiclient_http
from arvados.api import OrderedJsonModel

from arvados_testutil import fake_httplib2_response

if not mimetypes.inited:
    mimetypes.init()

class ArvadosApiTest(run_test_server.TestCaseWithServers):
    MAIN_SERVER = {}
    ERROR_HEADERS = {'Content-Type': mimetypes.types_map['.json']}

    def api_error_response(self, code, *errors):
        return (fake_httplib2_response(code, **self.ERROR_HEADERS),
                json.dumps({'errors': errors,
                            'error_token': '1234567890+12345678'}))

    def test_new_api_objects_with_cache(self):
        clients = [arvados.api('v1', cache=True) for index in [0, 1]]
        self.assertIsNot(*clients)

    def test_empty_list(self):
        answer = arvados.api('v1').humans().list(
            filters=[['uuid', 'is', None]]).execute()
        self.assertEqual(answer['items_available'], len(answer['items']))

    def test_nonempty_list(self):
        answer = arvados.api('v1').collections().list().execute()
        self.assertNotEqual(0, answer['items_available'])
        self.assertNotEqual(0, len(answer['items']))

    def test_timestamp_inequality_filter(self):
        api = arvados.api('v1')
        new_item = api.specimens().create(body={}).execute()
        for operator, should_include in [
                ['<', False], ['>', False],
                ['<=', True], ['>=', True], ['=', True]]:
            response = api.specimens().list(filters=[
                ['created_at', operator, new_item['created_at']],
                # Also filter by uuid to ensure (if it matches) it's on page 0
                ['uuid', '=', new_item['uuid']]]).execute()
            uuids = [item['uuid'] for item in response['items']]
            did_include = new_item['uuid'] in uuids
            self.assertEqual(
                did_include, should_include,
                "'%s %s' filter should%s have matched '%s'" % (
                    operator, new_item['created_at'],
                    ('' if should_include else ' not'),
                    new_item['created_at']))

    def test_exceptions_include_errors(self):
        mock_responses = {
            'arvados.humans.get': self.api_error_response(
                422, "Bad UUID format", "Bad output format"),
            }
        req_builder = apiclient_http.RequestMockBuilder(mock_responses)
        api = arvados.api('v1', requestBuilder=req_builder)
        with self.assertRaises(apiclient_errors.HttpError) as err_ctx:
            api.humans().get(uuid='xyz-xyz-abcdef').execute()
        err_s = str(err_ctx.exception)
        for msg in ["Bad UUID format", "Bad output format"]:
            self.assertIn(msg, err_s)

    def test_exceptions_without_errors_have_basic_info(self):
        mock_responses = {
            'arvados.humans.delete': (
                fake_httplib2_response(500, **self.ERROR_HEADERS),
                "")
            }
        req_builder = apiclient_http.RequestMockBuilder(mock_responses)
        api = arvados.api('v1', requestBuilder=req_builder)
        with self.assertRaises(apiclient_errors.HttpError) as err_ctx:
            api.humans().delete(uuid='xyz-xyz-abcdef').execute()
        self.assertIn("500", str(err_ctx.exception))

    def test_request_too_large(self):
        api = arvados.api('v1')
        maxsize = api._rootDesc.get('maxRequestSize', 0)
        with self.assertRaises(apiclient_errors.MediaUploadSizeError):
            text = "X" * maxsize
            arvados.api('v1').collections().create(body={"manifest_text": text}).execute()

    def test_ordered_json_model(self):
        mock_responses = {
            'arvados.humans.get': (None, json.dumps(collections.OrderedDict(
                        (c, int(c, 16)) for c in string.hexdigits))),
            }
        req_builder = apiclient_http.RequestMockBuilder(mock_responses)
        api = arvados.api('v1',
                          requestBuilder=req_builder, model=OrderedJsonModel())
        result = api.humans().get(uuid='test').execute()
        self.assertEqual(string.hexdigits, ''.join(result.keys()))


if __name__ == '__main__':
    unittest.main()
