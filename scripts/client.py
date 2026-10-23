#!/usr/bin/env python3
# 脚本侧的鉴权 HTTP 客户端：注册（或登录）后复用同一个会话。
# 用 Python 而不是 curl：Windows 的 curl 会按本地代码页发送中文请求体。
# shell 脚本因此走 ASCII 账户名，并通过这两个环境变量把同一份凭据传进来。
import json, os, urllib.error, urllib.request, http.cookiejar

NAME = os.environ.get("STARSTACK_USER", "演示用户")
PWD = os.environ.get("STARSTACK_PASS", "starstack123")


class Client:
    def __init__(self, port=None):
        port = port or int(os.environ.get("PORT", 8787))
        self.base = "http://127.0.0.1:%d" % port
        jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

    def call(self, path, body=None, method=None, raw=None, headers=None):
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        h = {"Content-Type": "application/json"}
        h.update(headers or {})
        req = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with self.op.open(req, timeout=120) as r:
                t = r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read().decode() or "{}")
        return 200, json.loads(t) if t.startswith(("{", "[")) else t

    def login(self, name=None, pwd=None):
        name, pwd = name or NAME, pwd or PWD
        code, me = self.call("/api/auth/register", {"name": name, "password": pwd})
        if code != 200:
            code, me = self.call("/api/auth/login", {"name": name, "password": pwd})
        if code != 200:
            raise SystemExit("登录失败：%s" % me)
        return me

    def submit(self, fields):
        parts = []
        for k, v in fields.items():
            parts.append(('--X\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n'
                          % (k, v)).encode())
        parts.append(b"--X--\r\n")
        return self.call("/api/jobs", raw=b"".join(parts),
                         headers={"Content-Type": "multipart/form-data; boundary=X"})


if __name__ == "__main__":
    c = Client()
    me = c.login()
    print("%s id=%s 余额=%s" % (me["name"], me["id"], me["credits"]))
