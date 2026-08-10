fn main() {
    match campd::run() {
        Ok(code) => std::process::exit(code),
        Err(error) => {
            eprintln!("campd: {error}");
            std::process::exit(1);
        }
    }
}
