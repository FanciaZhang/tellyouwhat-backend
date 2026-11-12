CREATE TABLE prompt_eval_recording_inputs (
 run_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 item_index INT NOT NULL,
 part_index INT NOT NULL,
 payload MEDIUMBLOB NOT NULL,
 nonce VARBINARY(32) NOT NULL,
 PRIMARY KEY(run_id,item_index,part_index),
 FOREIGN KEY(run_id,item_index) REFERENCES prompt_eval_items(run_id,item_index) ON DELETE CASCADE
) ENGINE=InnoDB;
