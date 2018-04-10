/**
 * netfilter.h
 *
 * Handles receiving raw netfilter packets for the Untnagle Packet Daemon
 *
 * Copyright (c) 2018 Untangle, Inc.
 * All Rights Reserved
 */

/*--------------------------------------------------------------------------*/
extern int go_netfilter_callback(int mark,unsigned char* data, int len);
extern void go_child_startup(void);
extern void go_child_goodbye(void);
/*--------------------------------------------------------------------------*/
static struct nfq_q_handle		*nfqqh;
static struct nfq_handle		*nfqh;
static int						cfg_sock_buffer = 1048576;
static int						cfg_net_maxlen = 10240;
static int						cfg_net_buffer = 32768;
static int						cfg_net_queue = 1818;
/*--------------------------------------------------------------------------*/
static int netq_callback(struct nfq_q_handle *qh,struct nfgenmsg *nfmsg,struct nfq_data *nfad,void *data)
{
struct nfqnl_msg_packet_hdr		*hdr;
unsigned char					*rawpkt;
struct iphdr					*iphead;
int								rawlen;
int								omark,nmark;

// get the packet header and mark
hdr = nfq_get_msg_packet_hdr(nfad);
omark = nfq_get_nfmark(nfad);

// get the packet length and data
rawlen = nfq_get_payload(nfad,(unsigned char **)&rawpkt);

	// ignore packets with invalid length
	if (rawlen < (int)sizeof(struct iphdr))
	{
	nfq_set_verdict(qh,(hdr ? ntohl(hdr->packet_id) : 0),NF_ACCEPT,0,NULL);
	logmessage(LOG_WARNING,"Invalid length %d received\n",rawlen);
	return(0);
	}

// use the iphdr structure for parsing
iphead = (struct iphdr *)rawpkt;

// ignore everything except IPv4
if (iphead->version != 4) return(0);

// we only care about TCP and UDP
if ((iphead->protocol != IPPROTO_TCP) && (iphead->protocol != IPPROTO_UDP)) return(0);

// call the go handler function
nmark = go_netfilter_callback(omark,rawpkt,rawlen);

// set the verdict and the returned mark
nfq_set_verdict2(qh,(hdr ? ntohl(hdr->packet_id) : 0),NF_ACCEPT,nmark,0,NULL);

return(0);
}
/*--------------------------------------------------------------------------*/
static int netfilter_startup(void)
{
int		ret;

//open a new netfilter queue handler
nfqh = nfq_open();

	if (nfqh == NULL)
	{
	logmessage(LOG_ERR,"Error returned from nfq_open()\n");
	g_shutdown = 1;
	return(1);
	}

// unbind any existing queue handler
ret = nfq_unbind_pf(nfqh,AF_INET);

	if (ret < 0)
	{
	logmessage(LOG_ERR,"Error returned from nfq_unbind_pf()\n");
	g_shutdown = 1;
	return(2);
	}

// bind the queue handler for AF_INET
ret = nfq_bind_pf(nfqh,AF_INET);

	if (ret < 0)
	{
	logmessage(LOG_ERR,"Error returned from nfq_bind_pf(lan)\n");
	g_shutdown = 1;
	return(3);
	}

// create a new netfilter queue
nfqqh = nfq_create_queue(nfqh,cfg_net_queue,netq_callback,NULL);

	if (nfqqh == 0)
	{
	logmessage(LOG_ERR,"Error returned from nfq_create_queue(%u)\n",cfg_net_queue);
	g_shutdown = 1;
	return(4);
	}

// set the queue length
ret = nfq_set_queue_maxlen(nfqqh,cfg_net_maxlen);

	if (ret < 0)
	{
	logmessage(LOG_ERR,"Error returned from nfq_set_queue_maxlen(%d)\n",cfg_net_maxlen);
	g_shutdown = 1;
	return(5);
	}

// set the queue data copy mode
ret = nfq_set_mode(nfqqh,NFQNL_COPY_PACKET,cfg_net_buffer);

	if (ret < 0)
	{
	logmessage(LOG_ERR,"Error returned from nfq_set_mode(NFQNL_COPY_PACKET)\n");
	g_shutdown = 1;
	return(6);
	}

return(0);
}
/*--------------------------------------------------------------------------*/
static void netfilter_shutdown(void)
{
// destroy the netfilter queue
nfq_destroy_queue(nfqqh);

// shut down the netfilter queue handler
nfq_close(nfqh);
}
/*--------------------------------------------------------------------------*/
static int netfilter_thread(void)
{
struct pollfd	network,console;
struct timeval	tv;
char			*buffer;
int				netsock;
int				val,ret;

gettimeofday(&g_runtime,NULL);

logmessage(LOG_INFO,"The netfilter thread is starting\n");

// allocate our packet buffer
buffer = (char *)malloc(cfg_net_buffer);

// call our netfilter startup function
ret = netfilter_startup();

	if (ret != 0)
	{
	logmessage(LOG_ERR,"Error %d returned from netfilter_startup()\n",ret);
	g_shutdown = 1;
	return(1);
	}

// get the socket descriptor for the netlink queue
netsock = nfnl_fd(nfq_nfnlh(nfqh));

	// set the socket receive buffer size if config value is not zero
	if (cfg_sock_buffer != 0)
	{
	val = cfg_sock_buffer;
	ret = setsockopt(netsock,SOL_SOCKET,SO_RCVBUF,&val,sizeof(val));

		if (ret != 0)
		{
		logmessage(LOG_ERR,"Error %d returned from setsockopt(SO_RCVBUF)\n",errno);
		g_shutdown = 1;
		return(1);
		}
	}

// set up the network poll structure
network.fd = netsock;
network.events = POLLIN;
network.revents = 0;

go_child_startup();

	while (g_shutdown == 0)
	{
	// wait for data on the socket
	ret = poll(&network,1,1000);

	// nothing received so just continue
	if (ret == 0) continue;

		// handle poll errors
		if (ret < 0)
		{
		if (errno == EINTR) continue;
		logmessage(LOG_ERR,"Error %d (%s) returned from poll()\n",errno,strerror(errno));
		break;
		}

		do
		{
		// read from the netfilter socket
		ret = recv(netsock,buffer,cfg_net_buffer,MSG_DONTWAIT);

			if (ret == 0)
			{
			logmessage(LOG_ERR,"The netfilter socket was unexpectedly closed\n");
			g_shutdown = 1;
			break;
			}

			if (ret < 0)
			{
			if ((errno == EAGAIN) || (errno == EINTR) || (errno == ENOBUFS)) break;
			logmessage(LOG_ERR,"Error %d (%s) returned from recv()\n",errno,strerror(errno));
			g_shutdown = 1;
			break;
			}

			// pass the data to the packet handler
			nfq_handle_packet(nfqh,buffer,ret);
		} while (ret > 0);
	}

// call our netfilter shutdown function
netfilter_shutdown();

// free our packet buffer memory
free(buffer);

logmessage(LOG_INFO,"The netfilter thread has terminated\n");
go_child_goodbye();
return(0);
}
/*--------------------------------------------------------------------------*/
static void netfilter_goodbye(void)
{
g_shutdown = 1;
}
/*--------------------------------------------------------------------------*/
