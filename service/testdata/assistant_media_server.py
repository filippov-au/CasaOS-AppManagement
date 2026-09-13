"""Disposable Newznab/NNTP fixture; serves metadata only, never real downloads."""
import email.utils
import http.server
import socketserver
import threading
import urllib.parse


class NNTP(socketserver.StreamRequestHandler):
    def handle(self):
        authenticated = False
        self.wfile.write(b"200 CasaOS test NNTP ready\r\n")
        while line := self.rfile.readline():
            command = line.decode().strip().upper()
            if command.startswith("AUTHINFO USER"):
                self.wfile.write(b"381 Password required\r\n")
            elif command == "AUTHINFO PASS FIXTURE-PASSWORD":
                authenticated = True
                self.wfile.write(b"281 Authentication accepted\r\n")
            elif command.startswith("AUTHINFO PASS"):
                self.wfile.write(b"481 Authentication rejected\r\n")
            elif command.startswith("ARTICLE "):
                # NZBGet probes a nonexistent article after connecting.
                self.wfile.write(b"430 No such article\r\n" if authenticated else b"480 Authentication required\r\n")
            elif command == "QUIT":
                self.wfile.write(b"205 Closing connection\r\n")
                break
            else:
                self.wfile.write(b"500 Not supported by test fixture\r\n")


class Newznab(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass  # API keys must not enter fixture logs.

    def do_GET(self):
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
        if query.get("apikey", ["fixture-key"])[0] != "fixture-key":
            body = '<error code="100" description="Incorrect API key"/>'
        elif query.get("t") == ["caps"]:
            body = '''<caps><server version="1.0" title="CasaOS test"/><limits max="100" default="100"/>
            <searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,tvdbid,season,ep"/></searching>
            <categories><category id="5000" name="TV"><subcat id="5030" name="TV/SD"/><subcat id="5040" name="TV/HD"/></category></categories></caps>'''
        else:
            body = f'''<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
            <channel><title>CasaOS fixture</title><description>Test only</description><link>http://fixture:8080</link>
            <newznab:response offset="0" total="1"/>
            <item><title>CasaOS.Fixture.S01E01.720p.HDTV.x264</title><guid>fixture-episode-1</guid><link>http://fixture:8080/fixture.nzb</link>
            <pubDate>{email.utils.formatdate(usegmt=True)}</pubDate><category>TV &gt; HD</category>
            <enclosure url="http://fixture:8080/fixture.nzb" length="100000000" type="application/x-nzb"/>
            <newznab:attr name="category" value="5040"/><newznab:attr name="size" value="100000000"/>
            </item></channel></rss>'''
        data = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/xml")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


socketserver.ThreadingTCPServer.allow_reuse_address = True
news = socketserver.ThreadingTCPServer(("0.0.0.0", 119), NNTP)
threading.Thread(target=news.serve_forever, daemon=True).start()
http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Newznab).serve_forever()
