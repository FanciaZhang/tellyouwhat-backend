INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
VALUES ('albums', '告你相册', 'cn.tellyouwhat.albums', '')
ON DUPLICATE KEY UPDATE app_id = apps.app_id;
