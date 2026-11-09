ALTER TABLE album_cloud_subscriptions
    DROP CHECK album_cloud_subscriptions_chk_2,
    ADD CONSTRAINT album_cloud_subscriptions_chk_2 CHECK (
        (product_id='albums.cloud.50gb.annual' AND quota_bytes=50000000000) OR
        (product_id='albums.cloud.500gb.annual' AND quota_bytes=500000000000) OR
        (environment='Sandbox' AND product_id='albums.cloud.50gb.annual.dev' AND quota_bytes=50000000000) OR
        (environment='Sandbox' AND product_id='albums.cloud.500gb.annual.dev' AND quota_bytes=500000000000)
    );
