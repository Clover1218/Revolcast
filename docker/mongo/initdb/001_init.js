// ============================================================
// Revolcast · MongoDB 初始化脚本（容器首次启动自动执行）
//
// 执行位置：/docker-entrypoint-initdb.d/001_init.js
// 执行时机：数据卷为空时由 mongo 镜像在启用鉴权后执行，仅执行一次。
//           修改下面的账号密码后需要 docker compose down -v 才会重新生效。
//
// 做两件事：
//   1) 在业务库（默认 revolcast）内创建业务账号。
//      后端连接串形如 mongodb://<user>:<pwd>@mongodb:27017/revolcast，
//      未指定 authSource 时默认按路径中的库鉴权，所以账号必须建在 revolcast 库内，
//      不能只在 admin 库建 root（root 仅用于运维/备份）。
//   2) 创建 server_messages 集合与索引（设计文档附录 B.2）。
//      game_events 集合的索引由后端启动时 EnsureIndexes 负责，此处不重复定义。
// ============================================================

const dbName = process.env.MONGO_APP_DB || 'revolcast';
const appUser = process.env.MONGO_APP_USER || 'revolcast';
const appPwd = process.env.MONGO_APP_PASSWORD || 'revolcast';

const target = db.getSiblingDB(dbName);

// ---- 1. 业务账号 ----
try {
  target.createUser({
    user: appUser,
    pwd: appPwd,
    roles: [{ role: 'readWrite', db: dbName }],
    mechanisms: ['SCRAM-SHA-256'],
  });
  print(`[revolcast-init] 已创建业务账号 ${appUser}@${dbName}`);
} catch (e) {
  print(`[revolcast-init] 创建业务账号跳过：${e.message}`);
}

// ---- 2. server_messages 集合与索引 ----
const existing = target.getCollectionNames();
if (existing.indexOf('server_messages') < 0) {
  target.createCollection('server_messages');
  print('[revolcast-init] 已创建集合 server_messages');
}
target.server_messages.createIndexes([
  { key: { server_id: 1, seq: -1 }, name: 'ix_server_messages_server_seq' },
  { key: { server_id: 1, created_at: -1 }, name: 'ix_server_messages_server_created' },
  { key: { sender_user_id: 1, created_at: -1 }, name: 'ix_server_messages_sender_created', sparse: true },
]);
print('[revolcast-init] server_messages 索引就绪');
