import type { NextConfig } from "next";

// 浏览器只打同源的 Next 服务：/backend/* 由 Next 服务端转发给本机 daemon
// （7373），反代 3000 出去时用户无需直连 7373（CORS 也随之无关）。
// /ws 也走同源：Next 的 rewrite 代理（dev/多数 prod 版本）转发 WebSocket
// upgrade；若部署形态不支持，则在反代层加一条 /ws → 127.0.0.1:7373/ws
// （需 Upgrade/Connection 头透传）。
const nextConfig: NextConfig = {
  async rewrites() {
    return [
      { source: "/ws", destination: "http://localhost:7373/ws" },
      { source: "/backend/:path*", destination: "http://localhost:7373/:path*" },
    ];
  },
};

export default nextConfig;
