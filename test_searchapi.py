import json
import sys
import urllib.parse
import urllib.request


def fetch(url: str):
    req = urllib.request.Request(url, headers={"User-Agent": "searchapi-test/1.0"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read()
        ct = resp.headers.get("Content-Type", "")
        return resp.status, ct, body


def pretty_print(body: bytes):
    try:
        obj = json.loads(body.decode("utf-8", errors="replace"))
        print(json.dumps(obj, indent=2)[:4000])
        if isinstance(obj, list):
            print(f"\n# items: {len(obj)}")
    except Exception:
        print(body.decode("utf-8", errors="replace")[:4000])


def main():
    base = "http://localhost:3600/search"
    query = sys.argv[1] if len(sys.argv) > 1 else "darious"

    wrong = f"{base}?query={urllib.parse.quote(query)}&=relevance"
    correct = f"{base}?query={urllib.parse.quote(query)}&sortBy=relevance"

    for label, url in [("WRONG", wrong), ("CORRECT", correct)]:
        print(f"\n== {label} ==")
        print(url)
        try:
            status, ct, body = fetch(url)
            print(f"HTTP {status}  Content-Type: {ct}")
            pretty_print(body)
        except Exception as e:
            print("Request failed:", e)


if __name__ == "__main__":
    main()