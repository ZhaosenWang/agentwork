"use client";

import { useImStatus, useConnectFeishu, useDisconnectFeishu } from "@/lib/queries";
import { Button, PageHeader } from "@/components/ui";

// Settings: the IM connect module. The owner connects Feishu by scanning a
// QR code (SDK one-click app registration), then sends the bot one message —
// the receive target is captured and milestone notifications flow from then
// on (DESIGN.v2.md decision 2-14).
export default function SettingsPage() {
  const { data: im, isLoading } = useImStatus();
  const connect = useConnectFeishu();
  const disconnect = useDisconnectFeishu();

  const status = im?.status ?? "idle";

  return (
    <div className="p-8">
      <PageHeader title="设置" />

      <div className="max-w-xl">
        <h2 className="text-lg font-medium mb-2">飞书通知（IM 连接）</h2>
        <p className="text-sm text-gray-500 mb-4">
          里程碑事件（待审批 / 完成 / 合入 / 失败）会推送到飞书。扫码连接，之后你只需要在审批卡点出现。
        </p>

        {isLoading ? (
          <p className="text-gray-500">加载中…</p>
        ) : status === "connected" ? (
          <div className="rounded-lg border border-emerald-200 bg-emerald-50 p-4 space-y-2">
            <p className="text-emerald-800 font-medium">✅ 已连接飞书</p>
            <p className="text-xs text-emerald-700">
              应用 {im?.app_id ? `（${im.app_id.slice(0, 8)}…）` : ""} · 推送目标 {im?.receive_id?.slice(0, 12) || "未捕获"}
            </p>
            <div className="pt-2">
              <Button variant="danger" onClick={() => disconnect.mutate()} disabled={disconnect.isPending}>
                {disconnect.isPending ? "断开中…" : "断开连接"}
              </Button>
            </div>
          </div>
        ) : status === "waiting_qr" || status === "waiting_message" ? (
          <div className="rounded-lg border border-blue-200 bg-blue-50 p-4 space-y-3">
            <p className="text-blue-800 font-medium">
              {status === "waiting_qr" ? "📱 用飞书扫码完成连接" : "✅ 已扫码——给机器人发条消息完成连接"}
            </p>
            {im?.qr?.img_base64 ? (
              <img
                src={`data:image/png;base64,${im.qr.img_base64}`}
                alt="飞书连接二维码"
                className="w-56 h-56 bg-white p-2 rounded"
              />
            ) : (
              <p className="text-sm text-blue-600">二维码生成中…</p>
            )}
            <ol className="text-xs text-blue-700 space-y-1 list-decimal list-inside">
              <li>用飞书 App 扫码（或打开链接 {im?.qr?.url ? "在飞书里打开" : "…"}）</li>
              <li>授权后会自动创建应用并建立长连接</li>
              <li>给机器人发任意一条消息（如"hi"）——推送目标即被捕获</li>
            </ol>
            {status === "waiting_message" && (
              <p className="text-sm text-blue-800 font-medium">等待你在飞书里给机器人发消息…</p>
            )}
          </div>
        ) : status === "failed" ? (
          <div className="rounded-lg border border-red-200 bg-red-50 p-4 space-y-2">
            <p className="text-red-800 font-medium">连接失败</p>
            <p className="text-xs text-red-700 break-all">{im?.error || "未知错误"}</p>
            <Button onClick={() => connect.mutate()} disabled={connect.isPending}>
              {connect.isPending ? "重试中…" : "重新连接"}
            </Button>
          </div>
        ) : (
          <div className="rounded-lg border border-gray-200 p-4 space-y-3">
            <p className="text-sm text-gray-600">尚未连接飞书。连接后，任务完成、卡点待审、合入结果都会推到这里。</p>
            <Button onClick={() => connect.mutate()} disabled={connect.isPending}>
              {connect.isPending ? "连接中…" : "连接飞书"}
            </Button>
            {connect.isError && <p className="text-sm text-red-500">{String(connect.error)}</p>}
          </div>
        )}
      </div>
    </div>
  );
}
