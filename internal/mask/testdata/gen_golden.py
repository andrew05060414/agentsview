"""Generates golden.json for internal/mask (and the OpenCodex parity harness).

Fake secrets are never written as whole literals: every secret is a list of
fragments that the test harnesses join at run time, so secret scanners do not
see a complete key in the repository. In a case:

  in / out   list of elements
             "text"                literal text
             {"j": [frags...]}     the fragments joined (a secret value)
             {"ph": [frags...]}    the placeholder of the joined value
  tier       "parity": OpenCodex's redactPlainText must produce the same `out`
             "go":     Go-only behaviour (L1 gating, extra vendor rules); `oc`
                       says what OpenCodex does instead ("differs"/"unchanged")

Run:  python gen_golden.py > golden.json
"""
import json

cases = []


def case(cid, tier, inp, out, **extra):
    c = {"id": cid, "tier": tier, "in": inp, "out": out}
    c.update(extra)
    cases.append(c)


def J(*frags):
    return {"j": list(frags)}


def P(*frags):
    return {"ph": list(frags)}


def both(cid, pre, frags, post, **extra):
    """secret masked in `pre + secret + post`."""
    case(cid, "parity", [pre, J(*frags), post], [pre, P(*frags), post], **extra)


# ---- vendor formats (parity) ------------------------------------------------
AWS = ("AKIA", "IOSFODNN7", "EXAMPLE")
both("aws-key-plain", "aws id: ", AWS, " (do not share)")
both("aws-key-line-start", "", AWS, "\nnext line")
case("aws-key-inside-word", "parity", ["x", J(*AWS)], ["x", J(*AWS)])
case("aws-key-too-short", "parity", [J("AKIA", "SHORT123")], [J("AKIA", "SHORT123")])
GOOG = ("AIza", "SyD3mo", "-abcdefghijklmnopqrstuvwxyz01")
assert len("".join(GOOG)) == 39, len("".join(GOOG))
both("google-key-query", "https://x.example/api?key=", GOOG, "&alt=json")
both("google-key-json", '{"k":"', GOOG, '"}')
SK = ("sk-", "proj-", "AbCdEf0123456789", "GhIjKlMn")
both("sk-proj", "OPENAI=", SK, "\n")
SKANT = ("sk-", "ant-", "api03-", "Zx9Qw8Er7", "Ty6Ui5Op4", "As3Df2Gh1Jk0")
both("sk-ant", "export K=", SKANT, " && run")
SKLEG = ("sk-", "T3BlbkFJ", "0123456789abcdefghijklmn")
both("sk-legacy", "key is ", SKLEG, ".")
case("sk-too-short", "parity", ["sk-", "tooshort12345"], ["sk-", "tooshort12345"])
case("sk-inside-word", "parity", ["task-", "abcdefghijklmnopqrstuvwxyz"], ["task-", "abcdefghijklmnopqrstuvwxyz"])
GHP = ("ghp_", "ABCDEFGHIJKLMNOP", "QRSTUVWXYZ0123456789")
both("github-ghp", "token ", GHP, " ok")
GHPAT = ("github_pat_", "11ABCDEFG0", "abcdefghijklmnopqrstuvwxyz")
both("github-fine-grained", "GH=", GHPAT, ";")
GHO = ("gho_", "0123456789ABCDEF", "abcdef")
both("github-oauth", "oauth: ", GHO, "")
# ---- JWT, bearer ------------------------------------------------------------
JWT = ("eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "dBjftJeZ4CVPmB92K27uhbUJU1p1r")
both("jwt-plain", "cookie jwt=", JWT, "; path=/")
both("jwt-in-json", '{"id_token":"', JWT, '"}')
# empty signature followed by a space: no word boundary after the final dot, so neither side matches
case("jwt-empty-signature-unmatched", "parity", ["x ", J("eyJ", "hbGciOiJub25lIn0", ".", "eyJzdWIiOiJ4eHh4In0", "."), " y"],
     ["x ", J("eyJ", "hbGciOiJub25lIn0", ".", "eyJzdWIiOiJ4eHh4In0", "."), " y"])
BEAR = ("tok_", "Zq81Lm40Xv73Bn55Pw")
both("bearer-header", "Authorization: Bearer ", BEAR, "\r\nHost: x")
both("bearer-lowercase", "authorization: bearer ", BEAR, "")
both("bearer-in-curl", "curl -H 'Authorization: Bearer ", BEAR, "' https://x")
both("bearer-trailing-dot", "Bearer ", BEAR, ".")
case("bearer-short-value", "parity", ["Bearer ", J("shorttok")], ["Bearer ", J("shorttok")])
# ---- connection strings -----------------------------------------------------
both("conn-postgres", "postgres://app:", ("S3cr3t", "Pass99"), "@db.internal:5432/main")
both("conn-https-basic", "https://bot:", ("hunter", "2hunter2"), "@example.org/path")
both("conn-redis", "redis://default:", ("r3d1s", "Pw0rd"), "@10.0.0.5:6379")
case("conn-no-password", "parity", ["https://user@host/path ", "http://host:8080/x"], ["https://user@host/path ", "http://host:8080/x"])
case("conn-twice-same", "parity",
     ["a=postgres://u:", J("sharedpw", "9"), "@h1/d b=postgres://v:", J("sharedpw", "9"), "@h2/d"],
     ["a=postgres://u:", P("sharedpw", "9"), "@h1/d b=postgres://v:", P("sharedpw", "9"), "@h2/d"])
# ---- private key block ------------------------------------------------------
PK = ("-----BEGIN RSA PRIV", "ATE KEY-----\nQUJDREVGR0hJSktMTU5PUA==\nUVJTVFVWV1hZWjAxMjM0NQ==\n-----END RSA PRIV", "ATE KEY-----")
both("private-key-rsa", "key:\n", PK, "\ndone")
PK2 = ("-----BEGIN PRIV", "ATE KEY-----\nQUJDREVGR0hJSg==\n-----END PRIV", "ATE KEY-----")
both("private-key-pkcs8", "", PK2, "")
PK3 = ("-----BEGIN OPENSSH PRIV", "ATE KEY-----\\nQUJDREVGR0g=\\n-----END OPENSSH PRIV", "ATE KEY-----")
both("private-key-escaped-newlines", '{"pem":"', PK3, '"}')
# ---- named assignment (random values: OpenCodex and L1 agree) ---------------
both("named-env", "API_KEY=", ("Xk3mP9qL", "2vN8wR5t"), "\nNEXT=1")
both("named-json-dq", '{"db_password": "', ("Tr0ub4dor", "3xYz"), '"}')
both("named-json-sq", "{'client_secret': '", ("aB3dE6gH", "9jK2mN5p"), "'}")
both("named-underscore-token", "export GITHUB_TOKEN=", ("v1.8f3a", "C9d2E7b1"), " ")
both("named-colon-space", "client_secret: ", ("QwErTy12", "34AsDf56"), chr(10))
both("named-in-url", "https://x/y?access_token=", ("Mn8Bv5Cx", "4Zq1Lk9J"), "&b=2")
both("named-camel", "{authToken:'", ("Gh7Jk4Lm", "2Np8Qr5S"), "'}")
case("named-value-too-short", "parity", ["api_key=", J("short12")], ["api_key=", J("short12")])
case("named-no-credential-name", "parity", ["user_id=", J("Ab3dEf6hIj9kLm")], ["user_id=", J("Ab3dEf6hIj9kLm")])
case("named-already-masked", "parity", ["api_key=", P("Xk3mP9qL", "2vN8wR5t")], ["api_key=", P("Xk3mP9qL", "2vN8wR5t")])
# ---- repetition / mixing / idempotency / unicode ----------------------------
case("same-secret-three-times", "parity",
     [J(*AWS), " and ", J(*AWS), " and ", J(*AWS)], [P(*AWS), " and ", P(*AWS), " and ", P(*AWS)])
case("two-different-secrets", "parity",
     ["a ", J(*AWS), " b ", J(*GHP)], ["a ", P(*AWS), " b ", P(*GHP)])
case("placeholder-untouched", "parity", ["see ", P(*AWS), " ok"], ["see ", P(*AWS), " ok"])
both("cjk-context", "密钥是 ", SK, " 请勿外传")
both("secret-then-newline-crlf", "x=", GOOG, "\r\n")
case("plain-text-unchanged", "parity", ["hello world, nothing secret here: 2026-10-27 12:00:00 id=42"],
     ["hello world, nothing secret here: 2026-10-27 12:00:00 id=42"])
case("empty", "parity", [""], [""])

# ---- Go-only: L1 gate on named assignments (OpenCodex masks all of these) ---
for cid, name, val in [
    ("gate-ident-value", "access_token=", ("my_secret", "_value_ok")),
    ("gate-path-value", "my_secret_file=", ("keys/prod", "/id_rsa_prod")),
    ("gate-uuid-value", "idempotency_key=", ("3f2b8c1e-", "9a4d-4e7b-", "b1c2-0d5e6f7a8b9c")),
    ("gate-digits-value", "session_key=", ("1234567890", "12")),
    ("gate-public-key-name", "public_key=", ("Qw3Rt6Yu", "9Io1Pa4S")),
    ("gate-name-contains-key-only", "monkey_id=", ("Qw3Rt6Yu", "9Io1Pa4S")),
]:
    case(cid, "go", [name, J(*val), "\n"], [name, J(*val), "\n"], oc="differs")
# a hex digest under a credential-like name is masked (both sides agree)
both("hex-digest-under-credential-name", "api_key_hash=", ("0123456789abcdef", "0123456789abcdef"), "\n")
# base64 values: '=' padding is part of the value, '/' does not make it a path
B64 = ("Z7QiLOOQ", "LDfhmmX8pP12BMur", "/jLfBMDn4JzrGF", "/vkMI", "=")
case("go-base64-padded-quoted", "go", ["cursor_secret = \"", J(*B64), "\"\n"], ["cursor_secret = \"", P(*B64), "\"\n"], oc="unchanged")
case("go-base64-padded-env", "go", ["MY_API_KEY=", J(*B64), "\n"], ["MY_API_KEY=", P(*B64), "\n"], oc="differs")
# credential-looking names that OpenCodex does not match but L1 does
case("go-cookie-name", "go", ["session_cookie=", J("Zx3Cv6Bn", "9Mq2Wd5E"), "; HttpOnly"],
     ["session_cookie=", P("Zx3Cv6Bn", "9Mq2Wd5E"), "; HttpOnly"], oc="unchanged")
# bare credential names (nothing before the keyword): OpenCodex's pattern needs a character in front
for cid, name, val in [
    ("go-bare-password", "password=", ("Tr0ub4dor", "3xYz9")),
    ("go-bare-token-json", '{"token": "', ("Ab3dEf6h", "Ij9kLm2N")),
    ("go-bare-secret-colon", "secret: ", ("QwErTy12", "34AsDf56")),
]:
    post = '"}' if cid.endswith("json") else chr(10)
    case(cid, "go", [name, J(*val), post], [name, P(*val), post], oc="unchanged")
# ---- Go-only: extra vendor formats from AgentsView's rule set ---------------
GL = ("glpat-", "AbCdEfGhIjKlMnOpQrSt")
case("go-gitlab-pat", "go", ["tok ", J(*GL), " end"], ["tok ", P(*GL), " end"], oc="unchanged")
NPM = ("npm_", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789")
case("go-npm-token", "go", ["//registry:_authToken=", J(*NPM), "\n"], ["//registry:_authToken=", P(*NPM), "\n"], oc="differs")
HF = ("hf_", "AbCdEfGhIjKlMnOpQrStUvWxYz0123")
case("go-hf-token", "go", ["HF ", J(*HF)], ["HF ", P(*HF)], oc="unchanged")
SLACK = ("xoxb-", "123456789012-", "AbCdEfGhIj")
case("go-slack-token", "go", ["slack ", J(*SLACK), " end"], ["slack ", P(*SLACK), " end"], oc="unchanged")
STR = ("sk_live_", "AbCdEfGhIjKlMnOp12")
case("go-stripe-secret", "go", ["stripe ", J(*STR), "!"], ["stripe ", P(*STR), "!"], oc="unchanged")
STS = ("ASIA", "ABCDEFGHIJKLMNOP")
case("go-aws-sts", "go", ["sts ", J(*STS), " end"], ["sts ", P(*STS), " end"], oc="unchanged")
SG = ("SG.", "AbCdEfGhIjKlMnOpQrStUv", ".", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfG")
case("go-sendgrid", "go", ["mail ", J(*SG), " end"], ["mail ", P(*SG), " end"], oc="unchanged")

print(json.dumps({"version": 1, "cases": cases}, indent=1, ensure_ascii=False))
