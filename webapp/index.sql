-- 直接適用するが、ここに記載しておく
ALTER TABLE comments ADD INDEX idx_post_id_created_at (post_id, created_at);
ALTER TABLE posts ADD INDEX idx_created_at (created_at);
ALTER TABLE comments ADD INDEX idx_user_id (user_id);
ALTER TABLE posts ADD INDEX idx_user_id_created_at (user_id, created_at DESC);