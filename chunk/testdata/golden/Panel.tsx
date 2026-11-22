import React, { useEffect, useState } from "react";

type Props = { title: string; items: string[]; onPick?: (item: string) => void };

export function Panel({ title, items, onPick }: Props) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (query.length > 0) {
      setOpen(true);
    }
  }, [query]);

  const shown = items.filter((i) => i.toLowerCase().includes(query.toLowerCase()));

  return (
    <section className="panel">
      <h2>{title}</h2>
      <input value={query} onChange={(e) => setQuery(e.target.value)} />
      {open && (
        <ul>
          {shown.map((item) => (
            <li key={item} onClick={() => onPick?.(item)}>
              {item}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export const Empty = () => <p>Nothing here</p>;

export default class Legacy extends React.Component<Props> {
  state = { count: 0 };

  componentDidMount() {
    document.title = this.props.title;
  }

  render() {
    return (
      <div onClick={() => this.setState({ count: this.state.count + 1 })}>
        {this.props.title}: {this.state.count}
      </div>
    );
  }
}
