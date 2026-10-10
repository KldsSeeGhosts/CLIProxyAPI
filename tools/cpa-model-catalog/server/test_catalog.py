import copy
import json
from pathlib import Path
import tempfile
import threading
import unittest
from urllib.request import Request, urlopen
from urllib.error import HTTPError
from http.server import ThreadingHTTPServer
import catalog_server as c

class CatalogTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / 'catalog.json'
        self.doc = c.read_catalog(Path(__file__).with_name('catalog.json'))
        self.path.write_text(json.dumps(self.doc))
    def tearDown(self):
        self.tmp.cleanup()
    def test_publication_is_validated_and_revision_checked(self):
        candidate = self.path.with_name('candidate.json')
        original = self.path.read_bytes()
        bad = copy.deepcopy(self.doc)
        bad['models'][0]['pi']['baseUrl'] = 'https://unexpected.invalid'
        candidate.write_text(json.dumps(bad))
        with self.assertRaises(ValueError): c.publish(self.path, candidate, c.revision(self.doc))
        self.assertEqual(self.path.read_bytes(), original)
        changed = copy.deepcopy(self.doc)
        alias = copy.deepcopy(next(m for m in changed['models'] if m['id'] == 'devin/swe-2'))
        alias['id'] = 'devin/swe-2-high'
        changed['models'].append(alias)
        changed['profiles']['personal']['models'].append('devin/swe-2-high')
        candidate.write_text(json.dumps(changed))
        with self.assertRaises(ValueError): c.publish(self.path, candidate, 'wrong')
        self.assertEqual(self.path.read_bytes(), original)
        rev = c.publish(self.path, candidate, c.revision(self.doc))
        self.assertEqual(rev, c.revision(changed))
        self.assertTrue((self.path.parent / 'history' / (c.revision(self.doc) + '.json')).exists())
    def test_authenticated_profile_and_etag(self):
        server = ThreadingHTTPServer(('127.0.0.1', 0), c.handler_for(self.path, lambda: ['test-key']))
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        url = f'http://127.0.0.1:{server.server_port}/v1/catalog/pi?profile=personal'
        try:
            with self.assertRaises(HTTPError) as error: urlopen(url)
            self.assertEqual(error.exception.code, 401)
            error.exception.close()
            with urlopen(Request(url, headers={'Authorization': 'Bearer test-key'})) as response:
                result = json.load(response); etag = response.headers['ETag']
            self.assertEqual(len(result['models']), 11)
            with self.assertRaises(HTTPError) as error:
                urlopen(Request(url, headers={'Authorization': 'Bearer test-key', 'If-None-Match': etag}))
            self.assertEqual(error.exception.code, 304)
            error.exception.close()
            with self.assertRaises(HTTPError) as error:
                urlopen(Request(url, headers={'If-None-Match': etag}))
            self.assertEqual(error.exception.code, 401)
            error.exception.close()
        finally:
            server.shutdown(); server.server_close(); thread.join()

if __name__ == '__main__': unittest.main()
