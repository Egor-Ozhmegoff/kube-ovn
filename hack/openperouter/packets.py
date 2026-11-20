#!/usr/bin/env python3
import ipaddress,json,socket,struct,sys,time

def mac(value): return bytes.fromhex(value.replace(':',''))
def checksum(data):
 if len(data)%2:data+=b'\0'
 total=sum(struct.unpack('!%dH'%(len(data)//2),data))
 while total>>16:total=(total&65535)+(total>>16)
 return (~total)&65535

def is_icmp(frame,source,destination):
 if len(frame)<42 or frame[12:14]!=b'\x08\x00': return False
 ip=frame[14:];ihl=(ip[0]&15)*4
 return ip[9]==1 and ip[12:16]==socket.inet_aton(source) and ip[16:20]==socket.inet_aton(destination) and len(ip)>ihl and ip[ihl]==8

def is_icmp6(frame,source,destination,kind=128):
 if len(frame)<62 or frame[12:14]!=b'\x86\xdd':return False
 ip=frame[14:]
 return ip[6]==58 and ip[8:24]==socket.inet_pton(socket.AF_INET6,source) and ip[24:40]==socket.inet_pton(socket.AF_INET6,destination) and ip[40]==kind

command,interface=sys.argv[1:3]
sock=socket.socket(socket.AF_PACKET,socket.SOCK_RAW,socket.htons(3));sock.bind((interface,0))
if command=='custom':
 source,destination,token=sys.argv[3:6]
 sock.send(mac(destination)+mac(source)+b'\x88\xb5'+token.encode().ljust(46,b'.'))
elif command in ('arp','rarp'):
 source,destination,token=sys.argv[3:6]
 ether=b'\x08\x06' if command=='arp' else b'\x80\x35'
 opcode=1 if command=='arp' else 3
 body=struct.pack('!HHBBH',1,0x0800,6,4,opcode)+mac(source)+socket.inet_aton('10.101.1.200' if source.startswith('02:aa:') else '10.101.1.11')+mac(destination)+socket.inet_aton('10.101.1.12')+token.encode()
 sock.send(mac(destination)+mac(source)+ether+body)
elif command=='icmp':
 source_mac,dest_mac,source_ip,dest_ip=sys.argv[3:7]
 body=struct.pack('!BBHHH',8,0,0,1234,1)+b'KUBEOVN-OPR-RETURN'
 body=body[:2]+struct.pack('!H',checksum(body))+body[4:]
 header=struct.pack('!BBHHHBBH4s4s',0x45,0,20+len(body),1234,0,64,1,0,socket.inet_aton(source_ip),socket.inet_aton(dest_ip))
 header=header[:10]+struct.pack('!H',checksum(header))+header[12:]
 sock.send(mac(dest_mac)+mac(source_mac)+b'\x08\x00'+header+body)
elif command in ('icmp6','nd'):
 source_mac,dest_mac,source_ip,dest_ip=sys.argv[3:7]
 source=socket.inet_pton(socket.AF_INET6,source_ip);destination=socket.inet_pton(socket.AF_INET6,dest_ip)
 if command=='icmp6':body=struct.pack('!BBHHH',128,0,0,1234,1)+b'KUBEOVN-OPR-RETURN6'
 else:body=struct.pack('!BBHI',136,0,0,0x20000000)+source+b'\x02\x01'+mac(source_mac)
 pseudo=source+destination+struct.pack('!I3xB',len(body),58)
 body=body[:2]+struct.pack('!H',checksum(pseudo+body))+body[4:]
 header=struct.pack('!IHBB16s16s',6<<28,len(body),58,255 if command=='nd' else 64,source,destination)
 sock.send(mac(dest_mac)+mac(source_mac)+b'\x86\xdd'+header+body)
elif command=='capture':
 mode,needle,filename=sys.argv[3:6]
 frames=[];sock.settimeout(.1)
 print('READY',flush=True)
 deadline=time.monotonic()+3
 while time.monotonic()<deadline:
  try:frame,_=sock.recvfrom(65535)
  except socket.timeout:continue
  matched=False
  if mode=='custom':matched=frame[12:14]==b'\x88\xb5' and needle.encode() in frame[14:]
  elif mode=='arp-reply':matched=len(frame)>=42 and frame[12:14]==b'\x08\x06' and frame[20:22]==b'\x00\x02' and frame[28:32]==socket.inet_aton(needle.split(',')[0]) and frame[38:42]==socket.inet_aton(needle.split(',')[1])
  elif mode in ('arp','rarp'):matched=frame[12:14]==(b'\x08\x06' if mode=='arp' else b'\x80\x35') and needle.encode() in frame[14:]
  elif mode=='icmp':matched=is_icmp(frame,*needle.split(','))
  elif mode=='icmp6':matched=is_icmp6(frame,*needle.split(','))
  elif mode=='nd':matched=is_icmp6(frame,*needle.split(','),kind=136)
  elif mode in ('vxlan-icmp','vxlan-icmp6') and len(frame)>50 and frame[12:14]==b'\x08\x00':
   outer=frame[14:];ihl=(outer[0]&15)*4
   if outer[9]==17 and len(outer)>ihl+16:
    udp=outer[ihl:]
    if struct.unpack('!H',udp[2:4])[0]==4789:matched=(is_icmp6 if mode=='vxlan-icmp6' else is_icmp)(udp[16:],*needle.split(','))
  if matched:frames.append((time.time(),frame))
 with open(filename,'wb') as output:
  output.write(struct.pack('<IHHIIII',0xa1b2c3d4,2,4,0,0,65535,1))
  for stamp,frame in frames:
   output.write(struct.pack('<IIII',int(stamp),int(stamp%1*1000000),len(frame),len(frame)));output.write(frame)
 print(json.dumps({'count':len(frames),'capture':filename,'samples':[frame.hex() for _,frame in frames[:2]]}),flush=True)
else:raise ValueError(command)
