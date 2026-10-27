make a low cpu cost mpts to spts web application
source input may have like udp://239.2.2.2:1234?localaddr=192.168.10.10, udp://239.2.2.2:1234?localaddr=192.168.11.11
output may have like udp://239.2.2.2:1234?localaddr=192.168.12.12, udp://239.2.2.2:1234?localaddr=192.168.13.13


output can be UDP and RTP

user can enter source input manually or auto input based on localaddr.

user will select what programs they need in output.

output ip and port can be auto generated or manually.

In output spts we need program name from source and service id (optional)

Need guranteed that output spts will not change later if we restart the application.


make it simple and professional web application.

Use best programming language and tools but our priority is low resource and have to be very fast.

It will handle many mpts to spts.

use sqlite as database.


