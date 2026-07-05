import requests
s = requests.Session()
r1 = s.get("http://localhost:8080/install/index.php")
r2 = s.post("http://localhost:8080/install/index.php", data={"action": "license"})
r3 = s.post("http://localhost:8080/install/index.php", data={"action": "requirements_check"})
with open("examples/wasmtime-fcgi-mybb/req.html", "w", encoding='utf-8') as f:
    f.write(r3.text)
r4 = s.post("http://localhost:8080/install/index.php", data={"action": "database_info"})
with open("examples/wasmtime-fcgi-mybb/db.html", "w", encoding='utf-8') as f:
    f.write(r4.text)
