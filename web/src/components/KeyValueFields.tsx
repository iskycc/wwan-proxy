import { Button, Form, Input, Typography } from 'antd';
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';

export default function KeyValueFields({
  name,
  title,
  labels,
  help,
}: {
  name: string;
  title: string;
  labels: [string, string];
  help?: string;
}) {
  return (
    <div className="list-fields">
      <Typography.Text strong>{title}</Typography.Text>
      {help && <Typography.Paragraph type="secondary">{help}</Typography.Paragraph>}
      <Form.List name={name}>
        {(fields, { add, remove }) => (
          <>
            {fields.map((field) => (
              <div key={field.key} className="dynamic-row">
                <Form.Item
                  name={[field.name, 'key']}
                  label={labels[0]}
                  rules={[{ required: true, message: '请填写' + labels[0] }]}
                >
                  <Input aria-label={labels[0]} placeholder={labels[0]} />
                </Form.Item>
                <Form.Item
                  name={[field.name, 'value']}
                  label={labels[1]}
                  rules={[{ required: true, message: '请填写' + labels[1] }]}
                >
                  <Input aria-label={labels[1]} placeholder={labels[1]} />
                </Form.Item>
                <Button
                  danger
                  type="text"
                  icon={<DeleteOutlined />}
                  aria-label={'删除' + title}
                  onClick={() => remove(field.name)}
                />
              </div>
            ))}
            <Button
              type="dashed"
              icon={<PlusOutlined />}
              onClick={() => add({ key: '', value: '' })}
            >
              添加{title}
            </Button>
          </>
        )}
      </Form.List>
    </div>
  );
}
