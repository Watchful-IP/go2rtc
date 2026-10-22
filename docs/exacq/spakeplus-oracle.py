# Isolated ctypes oracle for pinned Linux x64 libevAPI 1.47.27.1.
# Synthetic values only. No EVAPI sessions, networking or configuration writes.
import ctypes as C, hashlib, json, pathlib
library=pathlib.Path('/usr/local/exacq/webservice/bin/libevAPI.so')
assert hashlib.sha256(library.read_bytes()).hexdigest()=='14bd776c0fb0c46b72f2d2034a04ea2fa6a5794d0876facb1fc20cf4bda13509', 'Oracle requires the pinned SDK binary'
lib=C.CDLL(str(library))
base=C.cast(lib.EVAPI_Init,C.c_void_p).value-0x36af80
P=C.c_void_p
refs=[]
def buf(n):
 b=C.create_string_buffer(n);refs.append(b);return C.addressof(b)
def fn(addr,ret,*args):return C.CFUNCTYPE(ret,*args)(base+addr)
replace=fn(0x163d50,P,P,C.c_size_t,C.c_size_t,P,C.c_size_t)
assign=fn(0x164f10,None,P,P)
def initstr(p):C.c_void_p.from_address(p).value=p+16;C.c_size_t.from_address(p+8).value=0;C.c_char.from_address(p+16).value=b'\0'
def string(data=b''):
 p=buf(32);initstr(p)
 if data:
  b=C.create_string_buffer(data);replace(p,0,0,C.addressof(b),len(data))
 return p
def getstr(p):return C.string_at(C.c_void_p.from_address(p).value,C.c_size_t.from_address(p+8).value)
def setstr(p,data):assign(p,string(data))
def scalar(data):
 p=buf(64);C.memmove(p,data,32);fn(0x234340,None,P)(p);return C.string_at(p,32)
def clamp(data):
 b=bytearray(data);b[0]&=248;b[31]=(b[31]&63)|64;return bytes(b)
def point(s):
 p=buf(256);b=C.create_string_buffer(s);fn(0x233d00,None,P,P)(p,C.addressof(b));return p
def encode(p):
 out=buf(32);fn(0x232fc0,None,P,P)(out,p);return C.string_at(out,32)
def mask(s):
 p=buf(256);b=C.create_string_buffer(s);fn(0x233a20,None,P,P,P)(p,C.addressof(b),base+0x6d33d0);return p
def add(a,b):
 cache=buf(256);tmp=buf(256);out=buf(256)
 fn(0x233690,None,P,P)(cache,b);fn(0x2337b0,None,P,P,P)(tmp,a,cache);fn(0x233750,None,P,P)(out,tmp);return out
def mul(s,p):
 out=buf(256);b=C.create_string_buffer(s);fn(0x233ef0,None,P,P,P)(out,C.addressof(b),p);return encode(out)
def hashfields(*fields):return hashlib.sha256(b''.join(str(len(f)).encode()+f for f in fields)).digest()
process=fn(0x25fda0,C.c_bool,P,P,P)
setblobs=fn(0x25e1d0,None,P,P,P)
fixtures=[]
for i in range(3):
 user=[b'native-fixture',b'',b'caf\xc3\xa9'][i];password=[b'fixture-password',b'password-with-\x00-byte',b'\xf0\x9f\x94\x91-test'][i];server=[b'ORACLE-SERIAL',b'',b'SERIAL-THREE'][i]
 salt=bytes(range(16+i));xraw=bytes(range(i,32+i));x=clamp(xraw);y=clamp(bytes(range(63-i,31-i,-1)))
 obj=buf(0x158)
 for off in [0x10,0x30,0x50,0x70,0x90,0xb0,0xd0,0x118,0x138]:initstr(obj+off)
 for off,data in [(0x10,user),(0x30,server),(0x50,password)]:setstr(obj+off,data)
 C.c_void_p.from_address(obj+0xf0).value=base+0x1f3500
 a,b=string(),string();assert process(obj,a,b);assert getstr(b)==user
 # Override generated randomness after checking the SDK's initial point.
 generated=C.string_at(obj+0xf8,32);assert getstr(a)==encode(point(generated))
 X=encode(point(x));C.memmove(obj+0xf8,x,32);setstr(obj+0x118,X)
 derived=string();fn(0x1f3500,P,P,P,P)(derived,string(password),string(salt));derived=getstr(derived)
 w0,w1=scalar(derived[:32]),scalar(derived[32:]);Y=encode(add(point(y),mask(w0)))
 Z=mul(y,point(x));V=mul(w1,point(y));expected_client=hashfields(user,X,Y,Z,V);expected_server=hashfields(server,X,Y,Z,V);session=hashfields(user,server,X,Y,Z,V)
 a,b=string(Y),string(salt);setblobs(obj,a,b);assert process(obj,a,b)
 assert getstr(a)==expected_client and getstr(b)==salt
 assert getstr(obj+0xd0)==expected_server and getstr(obj+0x90)==session
 setblobs(obj,string(bytes(32)),string());assert not process(obj,string(),string())
 setblobs(obj,string(expected_server),string());assert process(obj,string(),string());assert C.c_uint32.from_address(obj+8).value==3
 fixtures.append({'username':user.decode(),'password_hex':password.hex(),'server':server.decode(),'salt':salt.hex(),'private':xraw.hex(),'server_private':y.hex(),'mask_n':encode(mask((1).to_bytes(32,'little'))).hex(),'argon2':derived.hex(),'w0':w0.hex(),'w1':w1.hex(),'public':X.hex(),'peer':Y.hex(),'z':Z.hex(),'v':V.hex(),'client_proof':expected_client.hex(),'server_proof':expected_server.hex(),'session':session.hex()})
print(json.dumps({'sdk':'libevAPI.so 1.47.27.1','fixtures':fixtures},indent=2))
