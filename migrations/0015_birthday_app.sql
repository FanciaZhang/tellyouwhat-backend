INSERT INTO apps (app_id, display_name, bundle_id, managed_product_id)
SELECT 'birthday', '告你生日', 'cn.tellyouwhat.birthday', 'birthday.assistant'
WHERE NOT EXISTS (SELECT 1 FROM apps WHERE app_id = 'birthday');
