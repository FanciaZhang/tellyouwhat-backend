INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
VALUES ('birthday', '告你生日', 'cn.tellyouwhat.birthday', 'birthday.assistant')
ON DUPLICATE KEY UPDATE app_id = apps.app_id;
